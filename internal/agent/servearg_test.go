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
	got := buildOpencodeServeArgs([]string{"--model", "gpt-5"}, 9999)
	want := []string{
		"serve",
		"--model", "gpt-5",
		"--hostname", "127.0.0.1",
		"--port", "9999",
		"--print-logs",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildOpencodeServeArgs = %v, want %v", got, want)
	}
}
