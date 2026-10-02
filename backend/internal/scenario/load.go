package scenario

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

//go:embed schema/scenario.schema.json
var schemaJSON []byte

// SchemaJSON returns the embedded JSON Schema, e.g. for editors and docs.
func SchemaJSON() []byte { return bytes.Clone(schemaJSON) }

const (
	schemaURL    = "https://vortech.invalid/schemas/scenario/v1"
	maxFileBytes = 4 << 20
)

// sourceFiles lists every file of a scenario directory with the schema
// definition it must satisfy. Decoding order does not matter; cross-file
// references are validated afterwards.
var sourceFiles = []struct {
	name string
	def  string
}{
	{"scenario.yaml", "scenario"},
	{"company.yaml", "company"},
	{"departments.yaml", "departments"},
	{"employees.yaml", "employees"},
	{"network.yaml", "network"},
	{"assets.yaml", "assets"},
	{"world.yaml", "world"},
}

// Problem is one validation finding.
type Problem struct {
	File    string
	Path    string // JSON pointer or logical location within the file
	Message string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.File + ": " + p.Message
	}
	return p.File + ": " + p.Path + ": " + p.Message
}

// ValidationError aggregates every problem found in a scenario.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario is invalid (%d problem(s)):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.String())
	}
	return b.String()
}

var (
	schemaOnce sync.Once
	schemas    map[string]*jsonschema.Schema
	schemaErr  error
	printer    = message.NewPrinter(language.English)
)

func compiledSchemas() (map[string]*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
		if err != nil {
			schemaErr = fmt.Errorf("parse embedded schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if err := c.AddResource(schemaURL, doc); err != nil {
			schemaErr = fmt.Errorf("add schema: %w", err)
			return
		}
		schemas = make(map[string]*jsonschema.Schema, len(sourceFiles))
		for _, f := range sourceFiles {
			s, err := c.Compile(schemaURL + "#/$defs/" + f.def)
			if err != nil {
				schemaErr = fmt.Errorf("compile schema %s: %w", f.def, err)
				return
			}
			schemas[f.def] = s
		}
	})
	return schemas, schemaErr
}

// Load reads, schema-validates, decodes and cross-validates the scenario in
// dir. A *ValidationError lists every problem found.
func Load(dir string) (*Bundle, error) {
	sch, err := compiledSchemas()
	if err != nil {
		return nil, err
	}

	b := &Bundle{Dir: dir}
	var problems []Problem
	hash := sha256.New()

	for _, f := range sourceFiles {
		raw, err := readLimited(filepath.Join(dir, f.name))
		if err != nil {
			problems = append(problems, Problem{File: f.name, Message: err.Error()})
			continue
		}
		hash.Write([]byte(f.name))
		hash.Write([]byte{0})
		hash.Write(raw)
		hash.Write([]byte{0})

		doc, err := yamlToJSON(raw)
		if err != nil {
			problems = append(problems, Problem{File: f.name, Message: err.Error()})
			continue
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
		if err != nil {
			problems = append(problems, Problem{File: f.name, Message: err.Error()})
			continue
		}
		if err := sch[f.def].Validate(inst); err != nil {
			problems = append(problems, schemaProblems(f.name, err)...)
			continue
		}
		if err := decodeInto(b, f.def, doc); err != nil {
			problems = append(problems, Problem{File: f.name, Message: err.Error()})
		}
	}
	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}

	b.Hash = hex.EncodeToString(hash.Sum(nil))
	if problems := validateReferences(b); len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}
	return b, nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("file is missing")
		}
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxFileBytes)
	}
	return raw, nil
}

// yamlToJSON converts a single YAML document to JSON. The YAML library
// rejects alias-expansion bombs; multiple documents per file are refused.
func yamlToJSON(raw []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var v any
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("file must contain exactly one YAML document")
	}
	norm, err := normalize(v)
	if err != nil {
		return nil, err
	}
	// Top-level "x-" keys only hold YAML anchors for reuse; their content
	// has already been expanded where referenced.
	if m, ok := norm.(map[string]any); ok {
		for k := range m {
			if strings.HasPrefix(k, "x-") {
				delete(m, k)
			}
		}
	}
	return json.Marshal(norm)
}

// normalize converts YAML-decoded values into JSON-compatible ones.
func normalize(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			n, err := normalize(val)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("mapping keys must be strings (got %v)", k)
			}
			n, err := normalize(val)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			n, err := normalize(val)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case time.Time:
		return t.UTC().Format(time.RFC3339), nil
	default:
		return v, nil
	}
}

func schemaProblems(file string, err error) []Problem {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Problem{{File: file, Message: err.Error()}}
	}
	var out []Problem
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, Problem{
				File:    file,
				Path:    "/" + strings.Join(e.InstanceLocation, "/"),
				Message: e.ErrorKind.LocalizedString(printer),
			})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func decodeInto(b *Bundle, def string, doc []byte) error {
	strict := func(dst any) error {
		dec := json.NewDecoder(bytes.NewReader(doc))
		dec.DisallowUnknownFields()
		return dec.Decode(dst)
	}
	switch def {
	case "scenario":
		return strict(&b.Scenario)
	case "company":
		return strict(&b.Company)
	case "departments":
		var f departmentsFile
		if err := strict(&f); err != nil {
			return err
		}
		b.Departments = f.Departments
	case "employees":
		var f employeesFile
		if err := strict(&f); err != nil {
			return err
		}
		b.Employees, b.Services = f.Employees, f.ServiceAccounts
	case "network":
		var f networkFile
		if err := strict(&f); err != nil {
			return err
		}
		b.Networks = f.Networks
	case "assets":
		var f assetsFile
		if err := strict(&f); err != nil {
			return err
		}
		b.Assets, b.Relations = f.Assets, f.Relationships
	case "world":
		return strict(&b.World)
	default:
		return fmt.Errorf("unknown definition %q", def)
	}
	return nil
}
