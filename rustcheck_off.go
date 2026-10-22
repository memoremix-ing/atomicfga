//go:build !rustcheck

package atomicfga

// rustState is empty unless built with the rustcheck tag.
type rustState struct{}

func (*rustState) close() {}
