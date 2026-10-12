package quality

// Cancel stops camera's running test and waits for its analysis to end. It
// reports whether a running test was found; the run is then recorded as
// failed with the error "cancelled". A test that finishes on its own in the
// instant between the check and the cancel keeps its own result.
func (r *Runner) Cancel(camera string) bool {
	r.mu.Lock()
	e := r.runs[camera]
	if e == nil || e.run.State != StateRunning {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()
	e.cancel()
	<-e.done
	return true
}

// Forget cancels camera's running test, waits for it to stop, and drops the
// camera's result. It publishes no event: it is for a camera that no longer
// exists.
func (r *Runner) Forget(camera string) {
	r.mu.Lock()
	e := r.runs[camera]
	delete(r.runs, camera)
	r.mu.Unlock()
	if e == nil {
		return
	}
	e.cancel()
	<-e.done
}
