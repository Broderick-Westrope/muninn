package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/broderick-westrope/muninn/internal/cli"
)

func main() {
	err := cli.Execute()
	if err == nil {
		return
	}
	code := 1
	var exitErr *cli.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.Code
		err = exitErr.Err
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(code)
}
