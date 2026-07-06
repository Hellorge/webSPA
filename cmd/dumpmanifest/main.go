package main

import (
	"fmt"
	"os"

	"gogogo/modules/router"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: dumpmanifest <path>")
		os.Exit(1)
	}
	m, err := router.LoadV3(os.Args[1])
	if err != nil {
		fmt.Printf("load: %v\n", err)
		os.Exit(1)
	}
	for i := range m.Table {
		e := &m.Table[i]
		path := m.Paths[e.PathOffset : e.PathOffset+uint32(e.PathLen)]
		fmt.Printf("%2d: hash=%016x action=%d holeOffset=%d path=%q\n",
			i, e.PathHash, e.ActionID, e.HoleOffset, path)
	}
}
