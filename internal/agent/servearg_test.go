package agent

import (
	"reflect"
	"testing"
)

func TestBuildRovodevServeArgs_Default(t *testing.T) {
	got := buildRovodevServeArgs(nil, 51234)
	want := []string{"rovodev", "serve", "--disable-session-token", "51234"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRovodevServeArgs(nil) = %v, want %v", got, want)
	}
}

func TestBuildRovodevServeArgs_ExtraArgsInserted(t *testing.T) {
	got := buildRovodevServeArgs([]string{"--profile", "work"}, 51234)
	want := []string{
		"rovodev", "serve",
		"--profile", "work",
		"--disable-session-token", "51234",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildRovodevServeArgs = %v, want %v", got, want)
	}
}

func TestBuildOpencodeServeArgs_Default(t *testing.T) {
	got := buildOpencodeServeArgs(nil, 9999)
	want := []string{
		"serve",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs(nil) = %v, want %v", got, want)
	}
}

func TestBuildOpencodeServeArgs_ExtraArgsInserted(t *testing.T) {
	got := buildOpencodeServeArgs([]string{"--log-level", "DEBUG"}, 9999)
	want := []string{
		"serve",
		"--log-level", "DEBUG",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs = %v, want %v", got, want)
	}
}

func TestBuildOpencodeServeArgs_ModelArgsNotPassedToServe(t *testing.T) {
	got := buildOpencodeServeArgs([]string{"--model", "litellm/gpt-5.4-xhigh", "--log-level", "DEBUG"}, 9999)
	want := []string{
		"serve",
		"--log-level", "DEBUG",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs = %v, want %v", got, want)
	}
}

func TestOpencodeModelOverrideFromArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantProvider string
		wantModel    string
		wantErr      bool
	}{
		{
			name:         "long form",
			args:         []string{"--model", "litellm/gpt-5.4-xhigh"},
			wantProvider: "litellm",
			wantModel:    "gpt-5.4-xhigh",
		},
		{
			name:         "equals form",
			args:         []string{"--model=litellm/glm-5.2-xhigh"},
			wantProvider: "litellm",
			wantModel:    "glm-5.2-xhigh",
		},
		{
			name:         "short form",
			args:         []string{"-m", "litellm/kimi-k2.7-code"},
			wantProvider: "litellm",
			wantModel:    "kimi-k2.7-code",
		},
		{
			name:         "short equals form",
			args:         []string{"-m=litellm/gpt-5.4-xhigh"},
			wantProvider: "litellm",
			wantModel:    "gpt-5.4-xhigh",
		},
		{
			name: "absent",
			args: []string{"--log-level", "DEBUG"},
		},
		{
			name:    "missing value",
			args:    []string{"--model"},
			wantErr: true,
		},
		{
			name:    "invalid value",
			args:    []string{"--model", "gpt-5.4-xhigh"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := opencodeModelOverrideFromArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantProvider == "" && tt.wantModel == "" {
				if got != nil {
					t.Fatalf("expected nil model override, got %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected model override")
			}
			if got.providerID != tt.wantProvider || got.modelID != tt.wantModel {
				t.Fatalf("model override = %#v, want provider=%q model=%q", got, tt.wantProvider, tt.wantModel)
			}
		})
	}
}
