//! Sets of possible OpenFGA outcomes.
//!
//! OpenFGA resolves the operands of union, intersection and exclusion
//! concurrently and, for intersection and exclusion, answers with whichever
//! decisive operand arrives first. Which one arrives first decides the
//! response's cycle flag (and so the answer of an enclosing exclusion) and
//! which error is reported. Rather than pick one interleaving, every
//! resolution step here yields the set of responses OpenFGA could produce,
//! or a superset of it. The engine answers only when every member of the
//! final set gives the same API result; otherwise it refuses.

use crate::ApiError;

pub const T: u8 = 1; // allowed
pub const F: u8 = 2; // not allowed
pub const C: u8 = 4; // not allowed, cycle detected
pub const E: u8 = 8; // error
pub const ALL: u8 = T | F | C | E;

#[derive(Clone, Debug)]
pub struct Out {
    pub bits: u8,
    /// The error, if `E` is possible and its kind is known.
    pub err: Option<ApiError>,
    /// Different errors are possible.
    pub err_ambiguous: bool,
    /// Why some possibilities are unknown to this port.
    pub refused: Option<String>,
    /// The deepest resolution depth reached.
    pub depth: u32,
}

impl Out {
    fn of(bits: u8) -> Out {
        Out {
            bits,
            err: None,
            err_ambiguous: false,
            refused: None,
            depth: 0,
        }
    }
    pub fn t() -> Out {
        Out::of(T)
    }
    pub fn f() -> Out {
        Out::of(F)
    }
    pub fn c() -> Out {
        Out::of(C)
    }
    pub fn bool(b: bool) -> Out {
        if b {
            Out::t()
        } else {
            Out::f()
        }
    }
    pub fn e(err: ApiError) -> Out {
        Out {
            err: Some(err),
            ..Out::of(E)
        }
    }
    pub fn refused(why: impl Into<String>) -> Out {
        Out {
            refused: Some(why.into()),
            ..Out::of(ALL)
        }
    }

    pub fn has(&self, b: u8) -> bool {
        self.bits & b != 0
    }

    /// Carries errors and refusal reasons of `children` into `self`.
    fn inherit<'a>(mut self, children: impl IntoIterator<Item = &'a Out>) -> Out {
        let mut kinds: Vec<&ApiError> = vec![];
        for c in children {
            self.depth = self.depth.max(c.depth);
            if self.refused.is_none() {
                self.refused.clone_from(&c.refused);
            }
            if c.has(E) {
                match &c.err {
                    Some(e) => kinds.push(e),
                    None => self.err_ambiguous = true,
                }
                self.err_ambiguous |= c.err_ambiguous;
            }
        }
        if self.has(E) {
            self.err = kinds.first().map(|e| (*e).clone());
            self.err_ambiguous |= kinds.iter().any(|k| !k.same_kind(kinds[0]));
        }
        self
    }
}

/// `union`: true wins, then an error, then a cycle.
pub fn union(children: &[Out]) -> Out {
    let mut bits = 0;
    if children.iter().any(|c| c.has(T)) {
        bits |= T;
    }
    if children.iter().all(|c| c.has(F | C | E)) && children.iter().any(|c| c.has(E)) {
        bits |= E;
    }
    if children.iter().all(|c| c.has(F | C)) && children.iter().any(|c| c.has(C)) {
        bits |= C;
    }
    if children.iter().all(|c| c.has(F)) {
        bits |= F;
    }
    Out::of(bits).inherit(children)
}

/// `intersection`: the first operand that is false or cyclic decides;
/// otherwise an error, otherwise true.
pub fn intersection(children: &[Out]) -> Out {
    let mut bits = 0;
    if children.iter().all(|c| c.has(T)) {
        bits |= T;
    }
    if children.iter().all(|c| c.has(T | E)) && children.iter().any(|c| c.has(E)) {
        bits |= E;
    }
    if children.iter().any(|c| c.has(F)) {
        bits |= F;
    }
    if children.iter().any(|c| c.has(C)) {
        bits |= C;
    }
    Out::of(bits).inherit(children)
}

/// `exclusion`: a false or cyclic base, or a true or cyclic subtrahend,
/// decides (whichever arrives first); otherwise the base's error, the
/// subtrahend's error, or true.
pub fn exclusion(base: &Out, sub: &Out) -> Out {
    let mut bits = 0;
    for b in [T, F, C, E] {
        if !base.has(b) {
            continue;
        }
        for s in [T, F, C, E] {
            if !sub.has(s) {
                continue;
            }
            if b == F || b == C {
                bits |= b;
            }
            match s {
                T => bits |= F,
                C => bits |= C,
                _ => {}
            }
            let decided = b == F || b == C || s == T || s == C;
            if !decided {
                bits |= if b == E || s == E { E } else { T };
            }
        }
    }
    Out::of(bits).inherit([base, sub])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exclusion_with_a_cyclic_subtrahend_depends_on_timing() {
        // base true, subtrahend false-with-cycle: false.
        assert_eq!(exclusion(&Out::t(), &Out::c()).bits, C);
        // An intersection of a plain false and a cycle may report either.
        let i = intersection(&[Out::f(), Out::c()]);
        assert_eq!(i.bits, F | C);
        // ...so excluding it from true has no single answer upstream.
        assert_eq!(exclusion(&Out::t(), &i).bits, T | C);
    }

    #[test]
    fn decisive_values_absorb_unknowns() {
        assert_eq!(union(&[Out::t(), Out::refused("x")]).bits, T);
        assert_eq!(intersection(&[Out::f(), Out::t()]).bits, F);
        assert_eq!(union(&[]).bits, F);
        let e = union(&[Out::f(), Out::e(ApiError::TooComplex)]);
        assert_eq!(e.bits, E);
        assert!(!e.err_ambiguous);
    }
}
