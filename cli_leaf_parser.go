package main

import (
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// leafParser preserves the direct method-level test harness while using the
// same Cobra/pflag parser as the public command tree. Production invocations
// are first parsed by newCobraRootCommand, which owns help, required flags,
// unknown-flag rejection and usage errors; handlers only consume the canonical
// flag values forwarded by that tree.
type leafParser struct{ command *cobra.Command }

func newLeafParser(use string) *leafParser {
	return &leafParser{command: &cobra.Command{Use: use, SilenceErrors: true, SilenceUsage: true}}
}

func (p *leafParser) SetOutput(out io.Writer) { p.command.Flags().SetOutput(out) }
func (p *leafParser) String(name, value, usage string) *string {
	return p.command.Flags().String(name, value, usage)
}
func (p *leafParser) StringVar(target *string, name, value, usage string) {
	p.command.Flags().StringVar(target, name, value, usage)
}
func (p *leafParser) StringSlice(name string, value []string, usage string) *[]string {
	return p.command.Flags().StringSlice(name, value, usage)
}
func (p *leafParser) StringArray(name string, value []string, usage string) *[]string {
	return p.command.Flags().StringArray(name, value, usage)
}
func (p *leafParser) Bool(name string, value bool, usage string) *bool {
	return p.command.Flags().Bool(name, value, usage)
}
func (p *leafParser) Duration(name string, value time.Duration, usage string) *time.Duration {
	return p.command.Flags().Duration(name, value, usage)
}
func (p *leafParser) Parse(args []string) error  { return p.command.ParseFlags(args) }
func (p *leafParser) NArg() int                  { return len(p.command.Flags().Args()) }
func (p *leafParser) Arg(index int) string       { return p.command.Flags().Arg(index) }
func (p *leafParser) Args() []string             { return p.command.Flags().Args() }
func (p *leafParser) Changed(name string) bool   { return p.command.Flags().Changed(name) }
func (p *leafParser) Visit(fn func(*pflag.Flag)) { p.command.Flags().Visit(fn) }
