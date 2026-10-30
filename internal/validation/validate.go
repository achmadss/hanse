package validation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// problemRuleJSON is the rule of a request body that is not JSON at all.
const problemRuleJSON = "json"

// Problem is one reason an argument set is invalid: the JSON pointer of
// the offending value ("" is the whole argument object), the JSON Schema
// rule that failed, and a message for people. A Problem never holds an
// argument value.
type Problem struct {
	Path    string `json:"path"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// Validate checks args against op's argument schema and its `hanse:node`
// rules: every such value must name a known, enabled node that passes the
// argument's node filter (docs/configuration.md sections 6.3 and 6.4).
// Node status is not part of the check: a target-only node has no agent
// and is offline, yet a request may name it.
//
// A nil or empty args value means no arguments, which is valid for an
// operation without required arguments. A failure is an *herrors.Error
// with the invalid_arguments code; its details key "errors" holds one
// Problem-shaped object per problem.
func Validate(op *registry.Operation, nodes *registry.NodeRegistry, args json.RawMessage) error {
	if op == nil {
		return herrors.New(herrors.Internal, "argument validation: no operation is available", nil)
	}
	if op.Schema == nil {
		return herrors.New(herrors.Internal,
			"argument validation: operation "+quoteID(op.ID)+" has no compiled argument schema", nil)
	}
	value, ok := decodeArguments(args)
	if !ok {
		return invalidArguments("the arguments are not valid JSON",
			[]Problem{{Rule: problemRuleJSON, Message: "the request body is not a JSON value"}})
	}
	if err := op.Schema.Validate(value); err != nil {
		problems := schemaProblems(err)
		return invalidArguments("the arguments do not match the schema", problems)
	}
	problems := nodeProblems(op, nodes, value)
	if len(problems) == 0 {
		return nil
	}
	return invalidArguments("the arguments do not match the schema", problems)
}

// quoteID renders an operation ID for an internal message.
func quoteID(id string) string { return fmt.Sprintf("%q", id) }

// decodeArguments parses the request body. An absent body is an empty
// object, so an operation without required arguments accepts it. The
// jsonschema decoder keeps number precision and rejects trailing data
// exactly as schema compilation does.
func decodeArguments(args json.RawMessage) (any, bool) {
	if len(bytes.TrimSpace(args)) == 0 {
		return map[string]any{}, true
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return nil, false
	}
	return value, true
}

// invalidArguments builds the one error shape every argument failure uses.
func invalidArguments(message string, problems []Problem) error {
	sortProblems(problems)
	details := make([]any, len(problems))
	for i, p := range problems {
		details[i] = map[string]any{
			"path":    p.Path,
			"rule":    p.Rule,
			"message": p.Message,
		}
	}
	return herrors.New(herrors.InvalidArguments, message, map[string]any{"errors": details})
}

// sortProblems orders the problems by pointer and then rule, so that the
// same request always produces the same error details.
func sortProblems(problems []Problem) {
	sort.SliceStable(problems, func(i, j int) bool {
		if problems[i].Path != problems[j].Path {
			return problems[i].Path < problems[j].Path
		}
		if problems[i].Rule != problems[j].Rule {
			return problems[i].Rule < problems[j].Rule
		}
		return problems[i].Message < problems[j].Message
	})
}

// schemaProblems turns a compiled-schema validation failure into one
// Problem per leaf rule failure. Wrapper errors have no rule of their own
// and are flattened; a satisfied alternative of anyOf or oneOf is not an
// error, so those wrappers report one problem instead of every branch.
func schemaProblems(err error) []Problem {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Problem{{Rule: "schema", Message: "the arguments do not match the schema"}}
	}
	var problems []Problem
	collectProblems(ve, &problems)
	if len(problems) == 0 {
		problems = append(problems, Problem{Rule: "schema", Message: "the arguments do not match the schema"})
	}
	return problems
}

// collectProblems walks one validation error tree.
func collectProblems(ve *jsonschema.ValidationError, out *[]Problem) {
	switch k := ve.ErrorKind.(type) {
	case *kind.Schema:
		// The document-level wrapper: its causes are the real problems.
		collectCauses(ve, out)
	case *kind.Group:
		collectCauses(ve, out)
	case *kind.Reference:
		// A $ref wrapper: the failure belongs to the referenced schema.
		collectCauses(ve, out)
	case *kind.AllOf:
		// Every branch must pass, so every cause is a real problem.
		collectCauses(ve, out)
	case *kind.Required:
		for _, name := range k.Missing {
			*out = append(*out, Problem{
				Path:    pointer(append(clonePath(ve.InstanceLocation), name)),
				Rule:    ruleOf(ve),
				Message: fmt.Sprintf("missing required argument %q", name),
			})
		}
	case *kind.AdditionalProperties:
		for _, name := range k.Properties {
			*out = append(*out, Problem{
				Path:    pointer(append(clonePath(ve.InstanceLocation), name)),
				Rule:    ruleOf(ve),
				Message: fmt.Sprintf("unknown argument %q", name),
			})
		}
	case *kind.AnyOf:
		*out = append(*out, problemAt(ve, "must match at least one of the allowed schemas"))
	case *kind.OneOf:
		*out = append(*out, problemAt(ve, "must match exactly one of the allowed schemas"))
	default:
		*out = append(*out, problemAt(ve, plainMessage(ve.ErrorKind)))
	}
}

// collectCauses copies the causes of a wrapper error, and keeps a generic
// problem for a wrapper that carries no cause.
func collectCauses(ve *jsonschema.ValidationError, out *[]Problem) {
	if len(ve.Causes) == 0 {
		*out = append(*out, problemAt(ve, "the value does not match the schema"))
		return
	}
	for _, cause := range ve.Causes {
		collectProblems(cause, out)
	}
}

// problemAt builds a problem at the error's instance location.
func problemAt(ve *jsonschema.ValidationError, message string) Problem {
	return Problem{Path: pointer(ve.InstanceLocation), Rule: ruleOf(ve), Message: message}
}

// ruleOf names the JSON Schema keyword that failed.
func ruleOf(ve *jsonschema.ValidationError) string {
	path := ve.ErrorKind.KeywordPath()
	if len(path) == 0 {
		return "schema"
	}
	return path[0]
}

// plainMessage describes a leaf rule failure without repeating a value:
// restrictions come from the schema, never from the request.
func plainMessage(k jsonschema.ErrorKind) string {
	switch t := k.(type) {
	case *kind.Type:
		return fmt.Sprintf("must be of type %s, got %s", strings.Join(t.Want, " or "), t.Got)
	case *kind.Format:
		return fmt.Sprintf("must match the format %q", t.Want)
	case *kind.Enum:
		return "must be one of the allowed values"
	case *kind.Const:
		return "must equal the allowed value"
	case *kind.Pattern:
		return fmt.Sprintf("must match the pattern %q", t.Want)
	case *kind.MinLength:
		return fmt.Sprintf("must be at least %d characters long", t.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("must be at most %d characters long", t.Want)
	case *kind.MinItems:
		return fmt.Sprintf("must hold at least %d items", t.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("must hold at most %d items", t.Want)
	case *kind.MinContains:
		return fmt.Sprintf("must hold at least %d matching items", t.Want)
	case *kind.MaxContains:
		return fmt.Sprintf("must hold at most %d matching items", t.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("must hold at least %d arguments", t.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("must hold at most %d arguments", t.Want)
	case *kind.Minimum:
		return fmt.Sprintf("must be at least %s", t.Want.RatString())
	case *kind.Maximum:
		return fmt.Sprintf("must be at most %s", t.Want.RatString())
	case *kind.ExclusiveMinimum:
		return fmt.Sprintf("must be greater than %s", t.Want.RatString())
	case *kind.ExclusiveMaximum:
		return fmt.Sprintf("must be less than %s", t.Want.RatString())
	case *kind.MultipleOf:
		return fmt.Sprintf("must be a multiple of %s", t.Want.RatString())
	case *kind.UniqueItems:
		return "must not repeat an item"
	case *kind.Contains:
		return "must hold a matching item"
	case *kind.AdditionalItems:
		return "must not hold more items"
	case *kind.Not:
		return "must not match the forbidden schema"
	case *kind.FalseSchema:
		return "is not allowed here"
	case *kind.DependentRequired:
		return fmt.Sprintf("must also provide %s", quotedList(t.Missing))
	case *kind.Dependency:
		return fmt.Sprintf("must also provide %s", quotedList(t.Missing))
	case *kind.PropertyNames:
		return "an argument name is not allowed"
	case *kind.InvalidJsonValue:
		return "is not representable in JSON"
	case *kind.RefCycle:
		return "the schema refers to itself"
	}
	return fmt.Sprintf("does not satisfy the %q rule", firstKeyword(k))
}

// firstKeyword names an unknown error kind's keyword.
func firstKeyword(k jsonschema.ErrorKind) string {
	path := k.KeywordPath()
	if len(path) == 0 {
		return "schema"
	}
	return path[0]
}

// quotedList renders names for a message.
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

// clonePath copies a location so that appending does not touch the
// library's slice.
func clonePath(path []string) []string {
	return append([]string(nil), path...)
}

// pointer renders JSON pointer segments per RFC 6901. The empty pointer
// names the whole argument object.
func pointer(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	var b strings.Builder
	for _, segment := range segments {
		b.WriteByte('/')
		b.WriteString(escapePointer(segment))
	}
	return b.String()
}

// escapePointer escapes one JSON pointer segment.
func escapePointer(segment string) string {
	segment = strings.ReplaceAll(segment, "~", "~0")
	return strings.ReplaceAll(segment, "/", "~1")
}
