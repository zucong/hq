package main

import (
	"io"
)

func execute(args []string, out, errOut io.Writer) error {
	command := newCobraRootCommand(globalOptions{}, out, errOut)
	command.SetArgs(args)
	return command.Execute()
}
