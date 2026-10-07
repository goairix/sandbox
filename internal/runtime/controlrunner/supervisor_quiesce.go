package controlrunner

// finishExecution belongs to the original start/result owner. All callbacks,
// pipe users and the sole cmd.Wait finish before retirement is published. The
// admission snapshot and ownerDone close share s.mu, leaving no retiring gap.
func (s *Supervisor) finishExecution(e *Execution) {
	e.cancel()
	if e.watchdogDone != nil {
		<-e.watchdogDone
	}
	if e.request != nil {
		e.request.Close()
	}
	if e.result != nil {
		e.result.Close()
	}
	if e.control != nil {
		e.control.Close()
	}
	<-e.waitDone
	close(e.output)
	s.mu.Lock()
	close(e.ownerDone)
	if s.active[e.commandID] == e {
		delete(s.active, e.commandID)
	}
	s.publishActiveLocked()
	s.mu.Unlock()
}
