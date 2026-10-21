package bind

import (
	"context"
	"sync/atomic"
)

// Statement is one query of a pipeline.
type Statement struct {
	Query string
	Args  []any
}

// Pipeliner is implemented by backends that can send several queries in a
// single round trip, such as PostgreSQL's extended-protocol pipeline. Results
// and errors are positional.
type Pipeliner interface {
	Pipeline(ctx context.Context, statements []Statement) ([]*Result, []error)
}

// pending is a query waiting in a session's pipeline queue.
type pending struct {
	call  *Call
	ctx   context.Context
	sig   context.Context
	stmt  Statement
	taken atomic.Bool // claimed by a leader, or abandoned by its caller
	done  chan struct{}
	res   *Result
	err   error
}

func (p *pending) finish(res *Result, err error) {
	p.res, p.err = res, err
	close(p.done)
}

// pipelined queues a query. OpenFGA resolves checks with concurrent
// goroutines, which the original server spreads over pooled connections. On
// one transaction their queries are serialized instead; here, the queries
// that queue up while a round trip is in flight are sent together in the
// next one. The first caller to find the session idle becomes the leader and
// drains the queue; nothing waits for a batch to fill.
func (c *Call) pipelined(ctx context.Context, p Pipeliner, query string, args []any) (*Result, error) {
	s := c.session
	pr := &pending{call: c, ctx: ctx, sig: cancellation(ctx), stmt: Statement{query, args}, done: make(chan struct{})}
	s.qmu.Lock()
	s.queue = append(s.queue, pr)
	s.qmu.Unlock()
	for {
		select {
		case <-pr.done:
			return pr.res, pr.err
		default:
		}
		select {
		case <-pr.done:
			return pr.res, pr.err
		case s.sem <- struct{}{}:
			s.lead(p)
		case <-pr.sig.Done():
			if pr.taken.CompareAndSwap(false, true) {
				s.dropped.Add(1)
				return nil, pr.sig.Err()
			}
			<-pr.done
			return pr.res, pr.err
		}
	}
}

// lead sends queued queries in pipelines until the queue is empty, then
// releases the session.
func (s *Session) lead(p Pipeliner) {
	defer s.release()
	for {
		s.qmu.Lock()
		batch := s.queue
		s.queue = nil
		s.qmu.Unlock()
		if len(batch) == 0 {
			return
		}
		live := batch[:0]
		for _, pr := range batch {
			if !pr.taken.CompareAndSwap(false, true) {
				continue // abandoned by a canceled caller
			}
			if err := pr.call.refuse(pr.ctx, pr.sig); err != nil {
				s.dropped.Add(1)
				pr.finish(nil, err)
				continue
			}
			live = append(live, pr)
		}
		if len(live) == 0 {
			continue
		}
		s.statements.Add(int64(len(live)))
		s.roundTrips.Add(1)
		if len(live) == 1 {
			res, err := s.backend.Query(context.WithoutCancel(live[0].ctx), live[0].stmt.Query, live[0].stmt.Args)
			live[0].finish(res, s.observe(err))
			continue
		}
		stmts := make([]Statement, len(live))
		for i, pr := range live {
			stmts[i] = pr.stmt
		}
		results, errs := p.Pipeline(context.WithoutCancel(live[0].ctx), stmts)
		for i, pr := range live {
			pr.finish(results[i], s.observe(errs[i]))
		}
	}
}
