package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestRootHelpListsEveryCommand(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		code, out, _ := run(args...)
		if code != 0 {
			t.Fatalf("%v = %d", args, code)
		}
		for _, want := range []string{"Usage:", "backup", "inspect", "verify", "version", "Take a backup"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v output lacks %q:\n%s", args, want, out)
			}
		}
	}
}

func TestCommandHelpHasDescriptionExamplesAndFlags(t *testing.T) {
	for cmd, wants := range map[string][]string{
		"backup":  {"--server", "--key", "--blob-host", "--concurrency", "Examples:", "cinc-server-backup backup --server"},
		"inspect": {"Examples:", "cinc-server-backup inspect"},
		"verify":  {"Examples:", "cinc-server-backup verify"},
	} {
		for _, args := range [][]string{{cmd, "--help"}, {"help", cmd}} {
			code, out, _ := run(args...)
			if code != 0 {
				t.Fatalf("%v = %d", args, code)
			}
			for _, want := range wants {
				if !strings.Contains(out, want) {
					t.Errorf("%v output lacks %q:\n%s", args, want, out)
				}
			}
		}
	}
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := run("version")
	if code != 0 || !strings.Contains(out, "cinc-server-backup "+Version) {
		t.Fatalf("version = %d %q", code, out)
	}
}

func TestUnknownFlagOrCommandIsUsageErrorPointingAtHelp(t *testing.T) {
	for _, args := range [][]string{{"backup", "--nope"}, {"frobnicate"}} {
		code, _, errOut := run(args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v = %d, stderr %q; want 2 and a pointer to --help", args, code, errOut)
		}
	}
}

func TestMissingArgumentIsUsageError(t *testing.T) {
	for _, args := range [][]string{{"inspect"}, {"verify", "a", "b"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v = %d, want 2", args, code)
		}
	}
}

// cinc-cli's house style: no em dashes in help text.
func TestHelpTextHasNoEmDashes(t *testing.T) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for field, text := range map[string]string{"Short": c.Short, "Long": c.Long, "Example": c.Example} {
			if strings.Contains(text, "\u2014") {
				t.Errorf("%s %s contains an em dash", c.CommandPath(), field)
			}
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if strings.Contains(f.Usage, "\u2014") {
				t.Errorf("%s --%s usage contains an em dash", c.CommandPath(), f.Name)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())
}
