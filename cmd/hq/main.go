package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var doctorFailed DoctorFailedError
		if errors.As(err, &doctorFailed) {
			os.Exit(exitCodeForError(err))
		}
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(exitCodeForError(err))
	}
}
