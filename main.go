// Command doze gives each dev server a <name>.localhost URL.
package main

import "os"

func main() { os.Exit(Main(os.Args[1:])) }
