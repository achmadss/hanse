package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/achmadss/hanse/internal/client"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// The run command builds one flag per argument of the operation's schema at
// run time (docs/architecture.md section 12). A new capability therefore
// works with no CLI change. Only scalars and arrays of scalars get a flag;
// a nested object needs --args-json or --args-file, because a command line
// carries no structure.

// argKind is the JSON type of one argument.
type argKind string

const (
	argString  argKind = "string"
	argInteger argKind = "integer"
	argNumber  argKind = "number"
	argBoolean argKind = "boolean"
	argArray   argKind = "array"
	// argObject covers every argument a flag cannot express: an object, an
	// array of arrays, and a schema without a usable type.
	argObject argKind = "object"
)

// reservedFlagNames are the run command's own flags. A schema argument with
// one of these names gets no flag, so the argument travels in --args-json or
// --args-file instead.
var reservedFlagNames = map[string]bool{
	"executor":  true,
	"password":  true,
	"detach":    true,
	"format":    true,
	"args-json": true,
	"args-file": true,
	"help":      true,
	"h":         true,
}

// runArgument is one dynamic argument flag.
type runArgument struct {
	name string
	flag *argFlag
}

// runFlags holds the flag set of one run invocation: the command's own
// flags and the argument flags of the fetched schema.
type runFlags struct {
	fs          *flag.FlagSet
	formatValue *string
	executor    *string
	password    *string
	detach      *bool
	argsJSON    *string
	argsFile    *string
	arguments   []runArgument
}

// newRunFlags builds the flag set of one operation. The argument flags are
// registered in schema property order, sorted by name, so the help output
// and the parse errors are deterministic.
func newRunFlags(describe client.Describe) (*runFlags, error) {
	fs := newFlagSet("run")
	rf := &runFlags{fs: fs}
	rf.formatValue = formatFlag(fs)
	rf.executor = fs.String("executor", "",
		"executor node ID (default: the Gateway's default executor)")
	rf.argsJSON = fs.String("args-json", "",
		"arguments as a JSON object; an explicit argument flag overrides its key")
	rf.argsFile = fs.String("args-file", "",
		"file holding the arguments as a JSON object, or - for standard input")
	rf.detach = fs.Bool("detach", false,
		"print the operation ID and return without waiting for the result")
	rf.password = fs.String("password", "",
		"password of a privileged operation; it is visible in the shell history")

	properties := schemaProperties(describe.Schema)
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if reservedFlagNames[name] {
			continue
		}
		property := properties[name]
		kind := schemaKind(property)
		element := argString
		if kind == argArray {
			element = arrayElementKind(property)
			if element == argObject || element == argArray {
				kind = argObject
			}
		}
		if kind == argObject {
			continue
		}
		argument := &argFlag{kind: kind, element: element}
		if kind == argArray {
			argument.enum = schemaEnum(property["items"])
		} else {
			argument.enum = schemaEnum(property)
		}
		required := containsString(describe.Required, name)
		fs.Var(argument, name, argumentUsage(property, kind, required))
		rf.arguments = append(rf.arguments, runArgument{name: name, flag: argument})
	}
	return rf, nil
}

// flagValueNames holds the value placeholder the help prints after each
// flag the run command defines itself. An argument flag uses its JSON type.
func flagValueNames() map[string]string {
	return map[string]string{
		"args-file": "path",
		"args-json": "object",
		"detach":    "",
		"executor":  "id",
		"format":    "text|json",
		"password":  "value",
	}
}

// argFlag is one argument flag. It keeps the raw values until the request is
// built, so an integer keeps every digit and an array keeps the order the
// user typed. The flag package calls Set once per occurrence.
type argFlag struct {
	kind    argKind
	element argKind
	enum    []string
	values  []string
}

// String implements flag.Value.
func (f *argFlag) String() string {
	if f == nil {
		return ""
	}
	return strings.Join(f.values, ",")
}

// Set implements flag.Value. It checks the value against its JSON type and
// against the schema's enum, so an obvious mistake fails before a request.
func (f *argFlag) Set(raw string) error {
	if f.kind == argArray {
		if err := validateScalar(f.element, raw); err != nil {
			return err
		}
	} else if err := validateScalar(f.kind, raw); err != nil {
		return err
	}
	if err := f.checkEnum(raw); err != nil {
		return err
	}
	if f.kind == argArray {
		f.values = append(f.values, raw)
		return nil
	}
	f.values = []string{raw}
	return nil
}

// IsBoolFlag lets a boolean argument be written without a value, as
// `--wait`, and still accept `--wait=false`.
func (f *argFlag) IsBoolFlag() bool { return f.kind == argBoolean }

// checkEnum rejects a value that the schema's enum does not list.
func (f *argFlag) checkEnum(raw string) error {
	if len(f.enum) == 0 {
		return nil
	}
	if containsString(f.enum, raw) {
		return nil
	}
	return fmt.Errorf("must be one of: %s", strings.Join(f.enum, ", "))
}

// usagePlaceholder returns the value name the help prints after the flag.
func (f *argFlag) usagePlaceholder() string {
	switch f.kind {
	case argBoolean:
		return ""
	case argArray:
		return string(f.element)
	default:
		return string(f.kind)
	}
}

// jsonValue renders the flag's values as a JSON value.
func (f *argFlag) jsonValue() (json.RawMessage, error) {
	if f.kind != argArray {
		if len(f.values) == 0 {
			return nil, errors.New("no value")
		}
		return scalarJSON(f.kind, f.values[len(f.values)-1])
	}
	items := make([]json.RawMessage, 0, len(f.values))
	for _, raw := range f.values {
		item, err := scalarJSON(f.element, raw)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return json.Marshal(items)
}

// scalarJSON renders one scalar value as JSON. A number goes out as the
// digits the user typed, not as a float, so a large integer keeps every
// digit.
func scalarJSON(kind argKind, raw string) (json.RawMessage, error) {
	switch kind {
	case argString:
		return json.Marshal(raw)
	case argInteger, argNumber:
		number, err := parseJSONNumber(raw)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(number.String()), nil
	case argBoolean:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, errors.New("must be true or false")
		}
		return json.Marshal(value)
	default:
		return nil, fmt.Errorf("unsupported argument type %q", kind)
	}
}

// validateScalar reports whether raw is a valid value of kind.
func validateScalar(kind argKind, raw string) error {
	switch kind {
	case argString:
		return nil
	case argInteger:
		number, err := parseJSONNumber(raw)
		if err != nil || strings.ContainsAny(number.String(), ".eE") {
			return errors.New("must be an integer")
		}
		return nil
	case argNumber:
		if _, err := parseJSONNumber(raw); err != nil {
			return errors.New("must be a number")
		}
		return nil
	case argBoolean:
		if _, err := strconv.ParseBool(raw); err != nil {
			return errors.New("must be true or false")
		}
		return nil
	default:
		return fmt.Errorf("unsupported argument type %q", kind)
	}
}

// parseJSONNumber reads one JSON number and nothing else. json.Number keeps
// the digits as written.
func parseJSONNumber(raw string) (json.Number, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", errors.New("the value has data after the number")
	}
	number, ok := value.(json.Number)
	if !ok {
		return "", errors.New("not a number")
	}
	return number, nil
}

// schemaKind returns the JSON type of one property. A union type uses its
// one non-null member; anything else is an object, which has no flag.
func schemaKind(property map[string]any) argKind {
	switch value := property["type"].(type) {
	case string:
		return mapKind(value)
	case []any:
		for _, item := range value {
			if name, ok := item.(string); ok && name != "null" {
				return mapKind(name)
			}
		}
	}
	return argObject
}

// mapKind maps a JSON Schema type name to an argKind.
func mapKind(name string) argKind {
	switch name {
	case "string":
		return argString
	case "integer":
		return argInteger
	case "number":
		return argNumber
	case "boolean":
		return argBoolean
	case "array":
		return argArray
	default:
		return argObject
	}
}

// arrayElementKind returns the type of the items of an array property. An
// undeclared item type accepts strings.
func arrayElementKind(property map[string]any) argKind {
	items, ok := property["items"].(map[string]any)
	if !ok {
		return argString
	}
	return schemaKind(items)
}

// schemaEnum returns the allowed values of a property as strings. An empty
// list means the schema sets no enum.
func schemaEnum(property any) []string {
	object, ok := property.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := object["enum"].([]any)
	if !ok {
		return nil
	}
	enum := make([]string, 0, len(raw))
	for _, item := range raw {
		enum = append(enum, enumString(item))
	}
	return enum
}

// enumString renders one enum value for comparison with a command line
// value.
func enumString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return "null"
	default:
		return fmt.Sprint(typed)
	}
}

// argumentUsage builds the help text of one argument flag. Required fields,
// enum values, and defaults show here, so --help answers what to pass.
func argumentUsage(property map[string]any, kind argKind, required bool) string {
	parts := make([]string, 0, 4)
	if description := schemaDescription(property); description != "" {
		parts = append(parts, description)
	}
	if required {
		parts = append(parts, "required")
	}
	enum := schemaEnum(property)
	if kind == argArray {
		enum = schemaEnum(property["items"])
	}
	if len(enum) > 0 {
		parts = append(parts, "one of: "+strings.Join(enum, ", "))
	}
	if format, ok := property["format"].(string); ok && format != "" {
		parts = append(parts, "format: "+format)
	}
	if value, ok := property["default"]; ok {
		parts = append(parts, "default: "+enumString(value))
	}
	if kind == argArray {
		parts = append(parts, "repeat the flag for each value")
	}
	if len(parts) == 0 {
		return "a value of the operation's schema"
	}
	return strings.Join(parts, "; ")
}

// containsString reports whether list holds value.
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// runArguments builds the request's arguments. --args-json or --args-file
// supplies the base object, and an explicit argument flag overrides the key
// of the same name. Neither source means no arguments.
func (a *app) runArguments(rf *runFlags) (json.RawMessage, error) {
	base := map[string]json.RawMessage{}
	jsonText := strings.TrimSpace(*rf.argsJSON)
	filePath := strings.TrimSpace(*rf.argsFile)
	switch {
	case jsonText != "" && filePath != "":
		return nil, usagef("pass --args-json or --args-file, not both")
	case filePath == "-":
		const maxArgsBytes = 1 << 20
		data, err := io.ReadAll(io.LimitReader(a.opts.Stdin, maxArgsBytes+1))
		if err != nil {
			return nil, usagef("cannot read the arguments from standard input: %v", err)
		}
		if len(data) > maxArgsBytes {
			return nil, usagef("the arguments are larger than %d bytes", maxArgsBytes)
		}
		jsonText = strings.TrimSpace(string(data))
	case filePath != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, usagef("cannot read the arguments file: %v", err)
		}
		jsonText = strings.TrimSpace(string(data))
	}
	if jsonText != "" {
		if err := checkArgumentsObject(jsonText); err != nil {
			return nil, usagef("%s", err)
		}
		if err := json.Unmarshal([]byte(jsonText), &base); err != nil {
			return nil, usagef("the arguments are not a JSON object: %v", err)
		}
	}
	for _, argument := range rf.arguments {
		if len(argument.flag.values) == 0 {
			continue
		}
		value, err := argument.flag.jsonValue()
		if err != nil {
			return nil, usagef("--%s: %v", argument.name, err)
		}
		base[argument.name] = value
	}
	if len(base) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return nil, usagef("the arguments could not be encoded: %v", err)
	}
	return encoded, nil
}

// checkRequired reports the required arguments that the request leaves
// out. The Gateway checks them too; this keeps a missing argument from
// becoming a round trip.
func checkRequired(describe client.Describe, arguments json.RawMessage) error {
	if len(describe.Required) == 0 {
		return nil
	}
	present := map[string]json.RawMessage{}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &present); err != nil {
			return usagef("the arguments are not a JSON object: %v", err)
		}
	}
	missing := make([]string, 0, len(describe.Required))
	for _, name := range describe.Required {
		value, ok := present[name]
		if !ok || len(value) == 0 || string(value) == "null" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	quoted := make([]string, len(missing))
	for i, name := range missing {
		quoted[i] = strconv.Quote(name)
	}
	message := "the required argument " + strings.Join(quoted, ", ") + " is missing"
	if len(quoted) > 1 {
		message = "the required arguments " + strings.Join(quoted, ", ") + " are missing"
	}
	return herrors.New(herrors.MissingArgument, message, map[string]any{"fields": missing})
}

// flagWasSet reports whether the command line set one flag.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
