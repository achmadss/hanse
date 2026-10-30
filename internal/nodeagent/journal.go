package nodeagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// journalVersion is the on-disk format version.
const journalVersion = 1

// journalFile is the run journal's file name inside the state directory
// (docs/agent-protocol.md section 8.1).
const journalFile = "journal.json"

// journal is the small on-disk record of every run the agent accepted and
// has not finished reporting. It removes an entry after the ack for the
// run's result, and after a restart its entries are reported as `lost`
// instead of being run again (docs/agent-protocol.md sections 4.1 and
// 8.1).
type journal struct {
	path string

	mu   sync.Mutex
	runs map[string]bool
}

// journalFileBody is the JSON shape of the journal file.
type journalFileBody struct {
	Version int      `json:"version"`
	Runs    []string `json:"runs"`
}

// openJournal loads the journal at path. A missing file is an empty
// journal. Every entry that survived a restart is a run whose outcome the
// agent lost, so the caller reports them lost and never runs them again.
func openJournal(path string) (*journal, error) {
	j := &journal{path: path, runs: map[string]bool{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the run journal: %w", err)
	}
	var body journalFileBody
	if err := json.Unmarshal(data, &body); err != nil {
		// A corrupt journal must not stop the agent: the runs it named
		// are unknown, and the Gateway resends a run whose ID is missing
		// from hello. Start a fresh journal.
		return j, nil
	}
	for _, id := range body.Runs {
		if id != "" {
			j.runs[id] = true
		}
	}
	return j, nil
}

// add records one accepted run.
func (j *journal) add(runID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.runs[runID] {
		return nil
	}
	j.runs[runID] = true
	return j.saveLocked()
}

// remove drops one run after the ack for its result.
func (j *journal) remove(runID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.runs[runID] {
		return nil
	}
	delete(j.runs, runID)
	return j.saveLocked()
}

// has reports whether the journal holds a run.
func (j *journal) has(runID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.runs[runID]
}

// lost returns every journal entry, in sorted order. After a restart every
// entry is a run whose outcome is lost.
func (j *journal) lost() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	ids := make([]string, 0, len(j.runs))
	for id := range j.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// saveLocked writes the journal through a temporary file, so a crash never
// leaves a half-written file. The journal mutex must be held.
func (j *journal) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return fmt.Errorf("cannot create the journal directory: %w", err)
	}
	body := journalFileBody{Version: journalVersion, Runs: make([]string, 0, len(j.runs))}
	for id := range j.runs {
		body.Runs = append(body.Runs, id)
	}
	sort.Strings(body.Runs)
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode the run journal: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".journal-*")
	if err != nil {
		return fmt.Errorf("cannot create the run journal: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write the run journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write the run journal: %w", err)
	}
	if err := os.Rename(name, j.path); err != nil {
		return fmt.Errorf("cannot store the run journal: %w", err)
	}
	return nil
}
