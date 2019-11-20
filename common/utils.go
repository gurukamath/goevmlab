// Copyright 2019 Martin Holst Swende
// This file is part of the goevmlab library.
//
// The library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the goevmlab library. If not, see <http://www.gnu.org/licenses/>.

package common

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"gopkg.in/urfave/cli.v1"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/holiman/goevmlab/evms"
	"github.com/holiman/goevmlab/fuzzing"
)

var (
	GethFlag = cli.StringFlag{
		Name:     "geth",
		Usage:    "Location of go-ethereum 'evm' binary",
		Required: true,
	}
	ParityFlag = cli.StringFlag{
		Name:     "parity",
		Usage:    "Location of go-ethereum 'parity-vm' binary",
		Required: true,
	}
	ThreadFlag = cli.IntFlag{
		Name:  "paralell",
		Usage: "Number of paralell executions to use.",
		Value: runtime.NumCPU(),
	}
	LocationFlag = cli.StringFlag{
		Name:  "outdir",
		Usage: "Location to place artefacts",
		Value: "/tmp",
	}
	PrefixFlag = cli.StringFlag{
		Name:  "prefix",
		Usage: "prefix of output files",
	}
	CountFlag = cli.IntFlag{
		Name:  "count",
		Usage: "number of tests to generate",
	}
)

type GeneratorFn func() *fuzzing.GstMaker

func ExecuteFuzzer(c *cli.Context, generatorFn GeneratorFn, name string) error {

	var (
		gethBin    = c.GlobalString(GethFlag.Name)
		parityBin  = c.GlobalString(ParityFlag.Name)
		numThreads = c.GlobalInt(ThreadFlag.Name)
		location   = c.GlobalString(LocationFlag.Name)
		numTests   uint64
	)
	fmt.Printf("numThreads: %d\n", numThreads)
	var wg sync.WaitGroup
	// The channel where we'll deliver tests
	testCh := make(chan string, 10)
	// The channel for cleanup-taksks
	removeCh := make(chan string, 10)
	// channel for signalling consensus errors
	consensusCh := make(chan string, 10)

	// Cancel ability
	sigs := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	abort := int64(0)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// Thread that creates tests, spits out filenames
	numFactories := numThreads / 2
	factories := int64(numFactories)
	for i := 0; i < numFactories; i++ {
		wg.Add(1)
		go func(threadId int) {
			defer wg.Done()
			defer func() {
				if f := atomic.AddInt64(&factories, -1); f == 0 {
					fmt.Printf("closing testCh\n")
					close(testCh)
				}
			}()
			for i := 0; atomic.LoadInt64(&abort) == 0; i++ {
				gstMaker := generatorFn()
				testName := fmt.Sprintf("%08d-%v-%d", i, name, threadId)
				test := gstMaker.ToGeneralStateTest(testName)
				fileName, err := storeTest(location, test, testName)
				if err != nil {
					fmt.Printf("Error: %v", err)
					break
				}
				testCh <- fileName
			}
		}(i)
	}
	executors := int64(0)

	evms := []evms.Evm{
		evms.NewGethEVM(gethBin),
		evms.NewParityVM(parityBin),
	}

	for i := 0; i < numThreads/2; i++ {
		// Thread that executes the tests and compares the outputs
		wg.Add(1)
		go func(threadId int) {
			defer wg.Done()
			atomic.AddInt64(&executors, 1)
			var outputs []*os.File
			defer func() {
				if f := atomic.AddInt64(&executors, -1); f == 0 {
					close(removeCh)
				}
			}()
			defer func() {
				for _, f := range outputs {
					f.Close()
				}
			}()
			// Open/create outputs for writing
			for _, evm := range evms {
				out, err := os.OpenFile(fmt.Sprintf("./%v-output-%d.jsonl", evm.Name(), threadId), os.O_CREATE|os.O_RDWR, 0755)
				if err != nil {
					fmt.Printf("failed opening file %v", err)
					return
				}
				outputs = append(outputs, out)
			}
			fmt.Printf("Fuzzing started \n")

			for file := range testCh {
				// Zero out the output files
				for _, f := range outputs {
					f.Truncate(0)
				}
				// Kick off the binaries
				var wg sync.WaitGroup
				wg.Add(len(evms))
				for i, evm := range evms {
					go func(out io.Writer) {
						evm.RunStateTest(file, out)
						wg.Done()
					}(outputs[i])
				}
				wg.Wait()
				// Seet to beginning
				for _, f := range outputs {
					f.Seek(0, 0)
				}
				atomic.AddUint64(&numTests, 1)
				// Compare outputs
				eq := compareFiles(outputs[0], outputs[1])
				if !eq {
					atomic.StoreInt64(&abort, 1)
					consensusCh <- file
					return
				} else {
					removeCh <- file
				}
			}
		}(i)
	}
	// One goroutine to spit out some statistics
	wg.Add(1)
	go func() {
		defer wg.Done()
		tStart := time.Now()
		ticker := time.NewTicker(5 * time.Second)
		testCount := uint64(0)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				n := atomic.LoadUint64(&numTests)
				testsSinceLastUpdate := n - testCount
				testCount = n
				timeSpent := time.Since(tStart)
				execPerSecond := float64(uint64(time.Second)*n) / float64(timeSpent)
				fmt.Printf("%d tests executed, in %v (%.02f tests/s)\n", n, timeSpent, execPerSecond)
				// Update global counter
				globalCount := uint64(0)
				if content, err := ioutil.ReadFile(".fuzzcounter"); err == nil {
					if count, err := strconv.Atoi((string(content))); err == nil {
						globalCount = uint64(count)
					}
				}
				globalCount += testsSinceLastUpdate

				ioutil.WriteFile(".fuzzcounter", []byte(fmt.Sprintf("%d", globalCount)), 0755)
			case <-ctx.Done():
				return
			}
		}

	}()
	// One goroutine to clean up after ourselves
	wg.Add(1)
	go func() {
		defer wg.Done()
		for path := range removeCh {
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "Error deleting file %v, : %v\n", path, err)
			}
		}
	}()

	select {
	case <-sigs:
	case path := <-consensusCh:
		fmt.Printf("Possible consensus error!\nFile: %v\n", path)
	}
	fmt.Printf("waiting for procs to exit\n")
	atomic.StoreInt64(&abort, 1)
	cancel()
	wg.Wait()
	return nil
}

func compareFiles(sf, df io.Reader) bool {
	sscan := bufio.NewScanner(sf)
	dscan := bufio.NewScanner(df)

	for sscan.Scan() {
		dscan.Scan()
		if !bytes.Equal(sscan.Bytes(), dscan.Bytes()) {
			fmt.Printf("diff: \nG: %v\nP: %v\n", string(sscan.Bytes()), string(dscan.Bytes()))
			return false
		}
	}
	return true
}

// storeTest saves a testcase to disk
func storeTest(location string, test *fuzzing.GeneralStateTest, testName string) (string, error) {

	fileName := fmt.Sprintf("%v.json", testName)
	fullPath := path.Join(location, fileName)

	f, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0755)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Write to file
	encoder := json.NewEncoder(f)
	if err = encoder.Encode(test); err != nil {
		return fullPath, err
	}
	return fullPath, nil
}
