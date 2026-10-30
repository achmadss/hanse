package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExecutorListUnmarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    ExecutorList
		wantErr string
	}{
		{
			name: "star",
			yaml: `"*"`,
			want: ExecutorList{Star: true},
		},
		{
			name: "list",
			yaml: `[node-a, node-b]`,
			want: ExecutorList{Nodes: []string{"node-a", "node-b"}},
		},
		{
			name: "single node listed as a list",
			yaml: `[node-a]`,
			want: ExecutorList{Nodes: []string{"node-a"}},
		},
		{
			name: "star inside a list decodes and is rejected by validation",
			yaml: `["*", node-a]`,
			want: ExecutorList{Nodes: []string{"*", "node-a"}},
		},
		{
			name:    "empty value",
			yaml:    `""`,
			wantErr: "must be \"*\" or a list of node IDs",
		},
		{
			name:    "map is not a list",
			yaml:    `{a: b}`,
			wantErr: "must be \"*\" or a list of node IDs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var e ExecutorList
			err := yaml.Unmarshal([]byte(tc.yaml), &e)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("decoded %q without an error", tc.yaml)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("yaml %q: %v", tc.yaml, err)
			}
			if got := e; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decoded %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExecutorListString(t *testing.T) {
	star := ExecutorList{Star: true}
	if star.String() != "*" {
		t.Errorf("star String() = %q, want *", star.String())
	}
	list := ExecutorList{Nodes: []string{"a", "b"}}
	if list.String() != "[a b]" {
		t.Errorf("list String() = %q, want [a b]", list.String())
	}
	if !(ExecutorList{}).IsEmpty() {
		t.Error("empty list IsEmpty() = false, want true")
	}
	if star.IsEmpty() {
		t.Error("star IsEmpty() = true, want false")
	}
	if (ExecutorList{Nodes: []string{"a"}}).IsEmpty() {
		t.Error("named list IsEmpty() = true, want false")
	}
}
