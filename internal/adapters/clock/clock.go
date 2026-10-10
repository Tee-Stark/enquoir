// Package clock implements app.Clock with the wall clock.
package clock

import "time"

// System is app.Clock backed by time.Now.
type System struct{}

func (System) Now() time.Time { return time.Now() }
