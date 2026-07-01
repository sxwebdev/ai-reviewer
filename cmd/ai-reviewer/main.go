// Command ai-reviewer is a local AI code-review tool for GitLab merge requests.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sxwebdev/ai-reviewer/internal/cli"
)

func main() {
	if err := cli.NewApp().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
