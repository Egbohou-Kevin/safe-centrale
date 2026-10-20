// Package schematest validates messages against the JSON Schemas in the schemas/
// folder. It is for tests only: it pulls in a third-party validator, so shipped
// code must never import it (the architecture tests enforce that).
package schematest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	once     sync.Once
	compiler *jsonschema.Compiler
	ids      map[string]string // file name -> $id
	loadErr  error
)

func dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "schemas")
}

func load() {
	compiler = jsonschema.NewCompiler()
	compiler.AssertFormat() // also check formats such as date-time
	ids = map[string]string{}

	files, err := filepath.Glob(filepath.Join(dir(), "*.json"))
	if err != nil || len(files) == 0 {
		loadErr = fmt.Errorf("no schema found in %s (%v)", dir(), err)
		return
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			loadErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			loadErr = fmt.Errorf("%s: %w", f, err)
			return
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if id == "" {
			loadErr = fmt.Errorf("%s: missing $id", f)
			return
		}
		if err := compiler.AddResource(id, doc); err != nil {
			loadErr = fmt.Errorf("%s: %w", f, err)
			return
		}
		ids[filepath.Base(f)] = id
	}
}

// Check validates a raw JSON message against the schema in a file of the schemas/
// folder (for example "alarm-ack.json"). It returns nil if the message is valid.
func Check(file string, raw []byte) error {
	once.Do(load)
	if loadErr != nil {
		return loadErr
	}
	id, ok := ids[file]
	if !ok {
		return fmt.Errorf("unknown schema %q", file)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		return fmt.Errorf("invalid schema %s: %w", file, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("unreadable message: %w", err)
	}
	return schema.Validate(inst)
}

// Validate marshals v to JSON and validates it against the schema. It fails the
// test if the message is not valid.
func Validate(t *testing.T, file string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(file, raw); err != nil {
		t.Errorf("%s rejects %s:\n%v", file, raw, err)
	}
}
