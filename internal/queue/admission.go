package queue

import "context"

// AdmissionController caps how many jobs this process's worker pool runs
// concurrently — task 6.1's "admission control." A simple counting
// semaphore is the honest interim; per-tenant fairness weighting is out of
// scope (README §2's commercial-tail collapse groups fairness/priority with
// the rest of the deferred scale-out infrastructure).
type AdmissionController struct {
	sem chan struct{}
}

func NewAdmissionController(maxConcurrent int) *AdmissionController {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	return &AdmissionController{sem: make(chan struct{}, maxConcurrent)}
}

// Acquire blocks until a slot is free or ctx is done, returning a release
// func to call exactly once when the caller's job finishes.
func (a *AdmissionController) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case a.sem <- struct{}{}:
		return func() { <-a.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
