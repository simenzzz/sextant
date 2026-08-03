package contracts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fixturesRoot points at the shared cross-language fixture corpus. The Go,
// Python, and TypeScript suites all run the same files, so a schema change
// without a matching fixture update fails all three at once instead of
// drifting quietly in one language.
const fixturesRoot = "../../../../packages/contracts/fixtures"

func TestSharedFixtureCorpus(t *testing.T) {
	entries, err := os.ReadDir(fixturesRoot)
	if err != nil {
		t.Fatalf("reading fixture corpus root: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("fixture corpus is empty")
	}

	seen := 0
	for _, schemaDir := range entries {
		if !schemaDir.IsDir() {
			continue
		}
		name := Name(schemaDir.Name())
		for _, kind := range []string{"valid", "invalid"} {
			dir := filepath.Join(fixturesRoot, schemaDir.Name(), kind)
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("contract %s has no %s/ fixtures: %v", name, kind, err)
			}
			if len(files) == 0 {
				t.Fatalf("contract %s: %s/ is empty", name, kind)
			}
			for _, f := range files {
				seen++
				t.Run(string(name)+"/"+kind+"/"+f.Name(), func(t *testing.T) {
					raw, err := os.ReadFile(filepath.Join(dir, f.Name()))
					if err != nil {
						t.Fatalf("reading fixture: %v", err)
					}
					verr := Validate(name, raw)
					if kind == "valid" && verr != nil {
						t.Errorf("expected valid, got: %v", verr)
					}
					if kind == "invalid" && verr == nil {
						t.Error("expected a validation failure, got none")
					}
				})
			}
		}
	}
	if seen == 0 {
		t.Fatal("fixture corpus contained no files")
	}
}

// Every contract the code names must have an embedded schema. Without this, a
// schema renamed in packages/contracts but not here fails at the first request
// rather than at build time.
func TestEveryDeclaredContractCompiles(t *testing.T) {
	declared := []Name{
		QuestionRequestV1, TraceEventV1, SQLPlanV1, CostLedgerV1, EvalResultV1,
		ParseSummaryV1, ResultSetV1,
	}
	for _, n := range declared {
		t.Run(string(n), func(t *testing.T) {
			err := Validate(n, []byte(`{}`))
			var unknown ErrUnknownContract
			if errors.As(err, &unknown) {
				t.Fatalf("contract %s has no embedded schema", n)
			}
			// `{}` is missing required fields for all five, so a validation
			// error is the expected outcome — what matters is that it is a
			// validation error and not an unknown-contract error.
			if err == nil {
				t.Errorf("Validate(%s, `{}`) = nil, want a required-field failure", n)
			}
		})
	}
	if got := len(Names()); got != len(declared) {
		t.Errorf("Names() = %d contracts, want %d — a schema was added without a constant", got, len(declared))
	}
}

func TestValidateRejectsUnknownContract(t *testing.T) {
	err := Validate(Name("not_a_contract.v9"), []byte(`{}`))
	var unknown ErrUnknownContract
	if !errors.As(err, &unknown) {
		t.Fatalf("Validate() error = %v, want ErrUnknownContract", err)
	}
}

func TestValidateRejectsMalformedJSON(t *testing.T) {
	err := Validate(TraceEventV1, []byte(`{"schema":`))
	var verr ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Validate() error = %v, want ValidationError", err)
	}
}
