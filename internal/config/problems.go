package config

import (
	"fmt"
	"strings"
)

// Problem is one configuration problem: the YAML file it came from, the
// field it concerns, and a sentence describing it. Every problem names the
// file path and the field, never a secret value.
type Problem struct {
	// File is the path of the configuration file the problem came from,
	// exactly as the loader received it.
	File string
	// Field is the dotted field path inside that file, for example
	// capabilities.pc.default_executor, or "file" when the file itself is
	// unreadable or unparsable.
	Field string
	// Message describes the problem.
	Message string
}

// Error implements the error interface.
func (p Problem) Error() string {
	if p.Field == "" {
		return fmt.Sprintf("%s: %s", p.File, p.Message)
	}
	return fmt.Sprintf("%s: %s: %s", p.File, p.Field, p.Message)
}

// Problems is a list of collected configuration problems. The loader
// collects every problem before reporting, never only the first.
type Problems []Problem

// Error aggregates all problems into one error, one line per problem.
func (ps Problems) Error() string {
	switch len(ps) {
	case 0:
		return ""
	case 1:
		return ps[0].Error()
	default:
		msgs := make([]string, 0, len(ps))
		for _, p := range ps {
			msgs = append(msgs, p.Error())
		}
		return fmt.Sprintf("%d configuration problems:\n%s", len(ps), strings.Join(msgs, "\n"))
	}
}
