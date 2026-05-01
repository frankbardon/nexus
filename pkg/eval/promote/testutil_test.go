package promote

import (
	"testing"

	evalcase "github.com/frankbardon/nexus/pkg/eval/case"
	"gopkg.in/yaml.v3"
)

// yamlUnmarshalForTest is the test-side mirror of yamlMarshal. Lives in
// testutil_test.go so production sources never directly import yaml.v3
// outside the centralized helpers in yaml.go.
func yamlUnmarshalForTest(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}

// loadCaseExternal exercises the real evalcase loader against a Promoted
// case directory. The success criterion is "loader does not error" — Phase
// 1's Load enforces every field's shape, so a green Load is the binding
// proof Promote produced a valid case.
func loadCaseExternal(t *testing.T, dir string) error {
	t.Helper()
	_, err := evalcase.Load(dir)
	return err
}
