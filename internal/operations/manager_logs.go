package operations

// Logs returns the stored log entries of one operation whose sequence
// number is greater than after, in sequence order (docs/architecture.md
// section 10). A store read failure is the Internal code, never an empty
// log.
func (m *Manager) Logs(id string, after int64) ([]LogEntry, error) {
	entries, err := m.logs.Entries(id, after)
	if err != nil {
		return nil, readFailed("read the log of operation "+id, err)
	}
	return entries, nil
}

// Follow subscribes to one operation's log before reading its backlog, so a
// follower that joins mid-run gets the earlier lines first and then the
// live lines, with no gap and no duplicate. Call Close on the returned
// subscription when done.
func (m *Manager) Follow(id string, after int64) (*LogSubscription, []LogEntry, error) {
	sub, backlog, err := m.logs.Follow(id, after)
	if err != nil {
		return nil, nil, readFailed("read the log of operation "+id, err)
	}
	return sub, backlog, nil
}

// Done returns a channel that closes when the operation's run ends. For an
// operation that already ended, or one this process never ran, the channel
// is already closed.
func (m *Manager) Done(id string) <-chan struct{} {
	m.mu.Lock()
	exec := m.live[id]
	m.mu.Unlock()
	if exec != nil {
		return exec.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}
