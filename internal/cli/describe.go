package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/achmadss/hanse/internal/client"
)

// describeCommand implements describe <capability> <operation>. It prints
// everything a client needs to build a valid request
// (docs/architecture.md section 11).
func (a *app) describeCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("describe")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("usage: hanse describe <capability> <operation>")
	}
	c, _, err := a.open(ctx)
	if err != nil {
		return err
	}
	describe, err := c.Describe(ctx, positional[0], positional[1])
	if err != nil {
		return err
	}
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, describe)
	}
	return a.writeDescribe(describe)
}

// writeDescribe prints one describe response as readable text. The argument
// table carries the schema's properties and required list, and the schema
// itself follows unchanged, so no detail is lost.
func (a *app) writeDescribe(describe client.Describe) error {
	rows := [][2]string{
		{"Capability", describe.Capability},
		{"Operation", describe.Operation},
		{"Description", describe.Description},
		{"Mode", describe.Mode},
		{"Privileged", describeBool(describe.Privileged)},
		{"Executor required", describeBool(describe.ExecutorRequired)},
	}
	if describe.DefaultExecutor != "" {
		rows = append(rows, [2]string{"Default executor", describe.DefaultExecutor})
	}
	if err := writeKeyValue(a.opts.Stdout, rows); err != nil {
		return err
	}

	properties := schemaProperties(describe.Schema)
	if len(properties) > 0 || len(describe.Required) > 0 {
		if err := a.writeArguments(properties, describe.Required); err != nil {
			return err
		}
	}
	if len(describe.Executors) > 0 {
		if err := a.writeExecutors(describe.Executors); err != nil {
			return err
		}
	}
	if len(describe.Options) > 0 {
		if err := a.writeOptionProviders(describe.Options); err != nil {
			return err
		}
	}
	if len(describe.NodeFilters) > 0 {
		if err := a.writeNodeFilters(describe.NodeFilters); err != nil {
			return err
		}
	}
	if describe.Schema == nil {
		return nil
	}
	if _, err := fmt.Fprintln(a.opts.Stdout, "\nSchema:"); err != nil {
		return err
	}
	return writeSchemaJSON(a.opts.Stdout, describe.Schema)
}

// writeArguments prints the argument table. A field that the schema does
// not describe is still listed when it is required, so a broken manifest
// shows rather than hides.
func (a *app) writeArguments(properties map[string]map[string]any, required []string) error {
	requiredSet := make(map[string]bool, len(required))
	names := make([]string, 0, len(properties)+len(required))
	for _, name := range required {
		requiredSet[name] = true
		if _, ok := properties[name]; !ok {
			names = append(names, name)
		}
	}
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)

	if _, err := fmt.Fprintln(a.opts.Stdout, "\nArguments:"); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "  FIELD\tTYPE\tREQUIRED\tDESCRIPTION"); err != nil {
		return err
	}
	for _, name := range names {
		property := properties[name]
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			oneLine(name), oneLine(schemaType(property)),
			describeBool(requiredSet[name]), oneLine(schemaDescription(property))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeExecutors prints the eligible executors and marks the default.
func (a *app) writeExecutors(executors []client.DescribeExecutor) error {
	if _, err := fmt.Fprintln(a.opts.Stdout, "\nExecutors:"); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "  ID\tSTATUS\tDEFAULT"); err != nil {
		return err
	}
	for _, executor := range executors {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n",
			oneLine(executor.ID), oneLine(executor.Status), describeBool(executor.Default)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// describeRefusesElevation reports whether the describe response knows the
// chosen executor and says the Gateway will refuse elevation for it, as it
// does for the host node of its embedded agent. An empty executor ID means
// the default executor. An executor the response does not list, or a
// response from a Gateway that does not send can_elevate, is left to the
// Gateway.
func describeRefusesElevation(describe client.Describe, executorID string) bool {
	if !describe.Privileged {
		return false
	}
	executorID = strings.TrimSpace(executorID)
	if executorID == "" {
		executorID = describe.DefaultExecutor
	}
	for _, executor := range describe.Executors {
		if executor.ID == executorID {
			return executor.CanElevate != nil && !*executor.CanElevate
		}
	}
	return false
}

// writeOptionProviders prints the arguments that have an option provider.
func (a *app) writeOptionProviders(options []client.DescribeOption) error {
	if _, err := fmt.Fprintln(a.opts.Stdout, "\nOptions:"); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "  FIELD\tPROVIDER"); err != nil {
		return err
	}
	for _, option := range options {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", oneLine(option.Field), oneLine(option.Provider)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeNodeFilters prints the filter of each `hanse:node` argument.
func (a *app) writeNodeFilters(filters map[string]client.DescribeNodeFilter) error {
	if _, err := fmt.Fprintln(a.opts.Stdout, "\nNode filters:"); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "  FIELD\tOS\tARCH\tROLES\tATTRIBUTES"); err != nil {
		return err
	}
	for _, field := range sortedKeys(filters) {
		filter := filters[field]
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			oneLine(field), oneLine(strings.Join(filter.OS, ", ")),
			oneLine(strings.Join(filter.Arch, ", ")), oneLine(strings.Join(filter.Roles, ", ")),
			oneLine(strings.Join(filter.Attributes, ", "))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// schemaProperties returns the object properties of a decoded schema, keyed
// by argument name. A schema that is not an object yields no properties.
func schemaProperties(schema any) map[string]map[string]any {
	out := map[string]map[string]any{}
	object, ok := schema.(map[string]any)
	if !ok {
		return out
	}
	raw, ok := object["properties"].(map[string]any)
	if !ok {
		return out
	}
	for name, value := range raw {
		if property, ok := value.(map[string]any); ok {
			out[name] = property
		}
	}
	return out
}

// schemaType renders an argument's type, with its format when it has one.
func schemaType(property map[string]any) string {
	kind := ""
	switch value := property["type"].(type) {
	case string:
		kind = value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				parts = append(parts, text)
			}
		}
		kind = strings.Join(parts, "|")
	}
	if format, ok := property["format"].(string); ok && format != "" {
		if kind == "" {
			return format
		}
		return kind + " (" + format + ")"
	}
	if kind == "" {
		return "-"
	}
	return kind
}

// schemaDescription returns an argument's description, or an empty string.
func schemaDescription(property map[string]any) string {
	description, _ := property["description"].(string)
	return description
}

// validateCommand implements validate <capability> <operation>. It sends
// the request to the Gateway, which checks it without running anything
// (docs/architecture.md sections 11 and 12).
func (a *app) validateCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("validate")
	formatValue := formatFlag(fs)
	executor := fs.String("executor", "", "executor node ID (optional)")
	argsJSON := fs.String("args-json", "", "arguments as a JSON object")
	argsFile := fs.String("args-file", "", "path to a file holding the arguments as a JSON object")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("usage: hanse validate <capability> <operation> [--executor <id>] [--args-json <object> | --args-file <path>]")
	}
	arguments, err := a.validationArguments(*argsJSON, *argsFile)
	if err != nil {
		return err
	}
	c, _, err := a.open(ctx)
	if err != nil {
		return err
	}
	result, err := c.Validate(ctx, client.ValidateRequest{
		Capability: positional[0],
		Operation:  positional[1],
		Arguments:  arguments,
		Executor:   strings.TrimSpace(*executor),
	})
	if err != nil {
		return err
	}
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, result)
	}
	return writeKeyValue(a.opts.Stdout, [][2]string{
		{"Valid", describeBool(result.Valid)},
		{"Capability", result.Capability},
		{"Operation", result.Operation},
		{"Mode", result.Mode},
		{"Executor", result.Executor},
		{"Executor source", result.ExecutorSource},
	})
}

// validationArguments returns the arguments to validate: the value of
// --args-json, or the contents of --args-file. Neither means no arguments.
// A value that is not one JSON object is a usage error, so a typo fails
// before any request.
func (a *app) validationArguments(jsonText, filePath string) (json.RawMessage, error) {
	jsonText = strings.TrimSpace(jsonText)
	filePath = strings.TrimSpace(filePath)
	switch {
	case jsonText != "" && filePath != "":
		return nil, usagef("pass --args-json or --args-file, not both")
	case filePath != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, usagef("cannot read the arguments file: %v", err)
		}
		jsonText = strings.TrimSpace(string(data))
	case jsonText == "":
		return nil, nil
	}
	if err := checkArgumentsObject(jsonText); err != nil {
		return nil, usagef("%s", err)
	}
	return json.RawMessage(jsonText), nil
}

// checkArgumentsObject requires text to be one JSON object. The Gateway
// checks the arguments again; this keeps an obvious typo from becoming a
// round trip.
func checkArgumentsObject(text string) error {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var object map[string]json.RawMessage
	if err := dec.Decode(&object); err != nil {
		return fmt.Errorf("the arguments are not a JSON object: %v", err)
	}
	if object == nil {
		return fmt.Errorf("the arguments must be a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the arguments have data after the JSON object")
	}
	return nil
}
