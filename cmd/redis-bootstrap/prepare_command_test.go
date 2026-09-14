package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/redisbootstrap"
)

func TestPrepareCommandFixedDefaultsAndOrdinalEnvironment(t *testing.T) {
	t.Setenv("POD_ORDINAL", "2")
	for _, args := range [][]string{{}, {"-ordinal", "2"}} {
		o, err := parsePrepareOptions(args)
		if err != nil || !reflect.DeepEqual(o, redisbootstrap.PreparePodOptions{Ordinal: 2, UID: 999, GID: 999}) {
			t.Fatal("safe default prep options not parsed", err)
		}
	}
	o, err := parsePrepareOptions([]string{"-ordinal", "1", "-uid", "1234", "-gid", "1235"})
	if err != nil || o.Ordinal != 1 || o.UID != 1234 || o.GID != 1235 {
		t.Fatal("explicit nonroot identity not parsed", err)
	}
}

func TestPrepareCommandRejectsExecutableAndUnsafeIdentity(t *testing.T) {
	t.Setenv("POD_ORDINAL", "1")
	for _, args := range [][]string{{"-seed-source", "/other/seed"}, {"-executable", "/bin/sh"}, {"-data-dir", "/host"}, {"-uid", "0"}, {"-gid", "0"}, {"-ordinal", "-1"}, {"-ordinal", "3"}, {"secret_do_not_echo"}} {
		if _, err := parsePrepareOptions(args); err == nil {
			t.Fatal("public fixed-path prep accepted unsafe input")
		}
	}
	for _, ordinal := range []string{"", "01", "+1", "-1", "3", "1\n"} {
		t.Setenv("POD_ORDINAL", ordinal)
		if _, err := parsePrepareOptions(nil); err == nil {
			t.Fatal("unstable ordinal env accepted")
		}
	}
}

func TestPrepareCommandStaticHelpAndCancellation(t *testing.T) {
	var output bytes.Buffer
	if err := runPrepare(context.Background(), []string{"--help"}, &output); err != nil || !strings.Contains(output.String(), "prepare-pod") {
		t.Fatal("static prepare help missing", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runPrepare(ctx, []string{"-ordinal", "1"}, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled prepare ignored", err)
	}
	output.Reset()
	if err := runPrepare(context.Background(), []string{"-executable", "secret_do_not_echo"}, &output); err == nil || strings.Contains(output.String(), "secret_do_not_echo") {
		t.Fatal("invalid argument printed")
	}
}
