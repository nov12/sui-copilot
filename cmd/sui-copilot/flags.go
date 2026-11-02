package main

import (
	"flag"
	"fmt"
	"strings"
)

func parseFlags(flags *flag.FlagSet, args []string) error {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") || argument == "-" || argument == "--" {
			break
		}
		name, _, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-"), "=")
		long := strings.HasPrefix(argument, "--")
		if len(name) > 1 && !long {
			return fmt.Errorf("invalid flag %q: use --%s", argument, name)
		}
		if len(name) == 1 && long {
			return fmt.Errorf("invalid flag %q: use -%s", argument, name)
		}
		option := flags.Lookup(name)
		if option == nil {
			break
		}
		boolean, isBoolean := option.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && !(isBoolean && boolean.IsBoolFlag()) {
			index++
		}
	}
	return flags.Parse(args)
}
