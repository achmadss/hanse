package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/achmadss/hanse/internal/client"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// operationsCommand implements operations, operations show, and operations
// cancel.
func (a *app) operationsCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("operations")
	formatValue := formatFlag(fs)
	capabilityFilter := fs.String("capability", "", "only operations of this capability")
	operationFilter := fs.String("operation", "", "only operations of this operation")
	executorFilter := fs.String("executor", "", "only operations on this executor")
	statusFilter := fs.String("status", "", "only operations in this state")
	requesterFilter := fs.String("requester", "", "only operations from this requester")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	filter := client.OperationFilter{
		Capability: *capabilityFilter,
		Operation:  *operationFilter,
		Executor:   *executorFilter,
		Status:     *statusFilter,
		Requester:  *requesterFilter,
	}
	if len(positional) > 0 {
		for _, filterFlag := range []struct {
			name  string
			value string
		}{
			{"--capability", *capabilityFilter},
			{"--operation", *operationFilter},
			{"--executor", *executorFilter},
			{"--status", *statusFilter},
			{"--requester", *requesterFilter},
		} {
			if filterFlag.value != "" {
				return usagef("%s filters the operation list only", filterFlag.name)
			}
		}
	}

	switch {
	case len(positional) == 0:
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		list, err := c.Operations(ctx, filter)
		if err != nil {
			return err
		}
		return a.writeOperations(fm, list)
	case len(positional) == 2 && positional[0] == "show":
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		operation, err := c.Operation(ctx, positional[1])
		if err != nil {
			return err
		}
		if fm == formatJSON {
			return writeJSON(a.opts.Stdout, client.OperationRecordEnvelope{Operation: operation})
		}
		return a.writeOperation(operation)
	case len(positional) == 2 && positional[0] == "cancel":
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		cancelled, err := c.CancelOperation(ctx, positional[1])
		if err != nil {
			return err
		}
		// The cancel endpoint answers with the record itself, without the
		// envelope of the get endpoint.
		if fm == formatJSON {
			return writeJSON(a.opts.Stdout, cancelled)
		}
		return a.writeOperation(cancelled)
	default:
		return usagef("usage: hanse operations [show <id> | cancel <id>]")
	}
}

// writeOperations prints the operation list.
func (a *app) writeOperations(fm format, list client.OperationList) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, list)
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "ID\tSTATUS\tCAPABILITY\tOPERATION\tEXECUTOR\tCREATED"); err != nil {
		return err
	}
	for _, operation := range list.Operations {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			oneLine(operation.ID), oneLine(operation.Status), oneLine(operation.Capability),
			oneLine(operation.Operation), oneLine(operation.Executor),
			operationTime(operation.CreatedAt)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeOperation prints one operation: its identity, state, executor,
// requester, times, and the result or error it ended with.
func (a *app) writeOperation(operation client.OperationRecord) error {
	rows := [][2]string{
		{"ID", operation.ID},
		{"Capability", operation.Capability},
		{"Operation", operation.Operation},
		{"Mode", operation.Mode},
		{"Status", operation.Status},
		{"Requester", operation.Requester},
		{"Executor", operation.Executor},
		{"Executor source", operation.ExecutorSource},
		{"Created", operationTime(operation.CreatedAt)},
		{"Started", operationTimePtr(operation.StartedAt)},
		{"Finished", operationTimePtr(operation.FinishedAt)},
	}
	if operation.Summary != "" {
		rows = append(rows, [2]string{"Summary", operation.Summary})
	}
	if err := writeKeyValue(a.opts.Stdout, rows); err != nil {
		return err
	}
	if len(operation.Result) > 0 {
		if _, err := fmt.Fprintln(a.opts.Stdout, "\nResult:"); err != nil {
			return err
		}
		if err := writeOperationResult(a.opts.Stdout, operation.Result); err != nil {
			return err
		}
	}
	if operation.Error != nil {
		if _, err := fmt.Fprintln(a.opts.Stdout, "\nError:"); err != nil {
			return err
		}
		rows := [][2]string{
			{"  Code", operation.Error.Code},
			{"  Message", operation.Error.Message},
		}
		if err := writeKeyValue(a.opts.Stdout, rows); err != nil {
			return err
		}
		if len(operation.Error.Details) > 0 {
			if _, err := fmt.Fprintln(a.opts.Stdout, "  Details:"); err != nil {
				return err
			}
			if err := writeJSON(a.opts.Stdout, operation.Error.Details); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeOperationResult prints one stored result. The Gateway serves the
// handler's result unchanged, so the CLI only adds indentation.
func writeOperationResult(w io.Writer, raw json.RawMessage) error {
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return herrors.New(herrors.Internal,
			"the operation result is not valid JSON: "+err.Error(), nil)
	}
	return writeJSON(w, value)
}

// operationTime renders one operation time in UTC. A zero time prints as
// a dash, so a row never shows a meaningless date.
func operationTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// operationTimePtr renders an optional operation time.
func operationTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return operationTime(*t)
}
