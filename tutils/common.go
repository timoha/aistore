// Package tutils provides common low-level utilities for all aistore unit and integration tests
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package tutils

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime/debug"
	"strconv"
	"testing"

	"github.com/NVIDIA/aistore/cluster"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/OneOfOne/xxhash"
)

const (
	letterBytes   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

func CheckFatal(err error, t *testing.T) {
	if err != nil {
		Logf("FATAL: %v\n", err)
		debug.PrintStack()
		t.Fatalf("FATAL: %v", err)
	}
}

func CheckError(err error, t *testing.T) {
	if err != nil {
		Logf("ERROR: %v\n", err)
		debug.PrintStack()
		t.Errorf("ERROR: %v", err)
	}
}

func Logln(msg string) {
	if testing.Verbose() {
		fmt.Fprintln(os.Stdout, msg)
	}
}

func Logf(msg string, args ...interface{}) {
	if testing.Verbose() {
		fmt.Fprintf(os.Stdout, msg, args...)
	}
}

func Progress(id int, period int) {
	if id > 0 && id%period == 0 {
		Logf("%3d: done.\n", id)
	}
}

func FastRandomFilename(src *rand.Rand, fnlen int) string {
	b := make([]byte, fnlen)
	// A src.Int63() generates 63 random bits, enough for letterIdxMax characters!
	for i, cache, remain := fnlen-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return string(b)
}

// Generates an object name that hashes to a different target than `baseName`.
func GenerateNotConflictingObjectName(baseName, newNamePrefix, bucketName string, smap *cluster.Smap) string {
	// Init digests - HrwTarget() requires it
	smap.InitDigests()

	newName := newNamePrefix

	baseNameHrw, _ := cluster.HrwTarget(bucketName, baseName, smap)
	newNameHrw, _ := cluster.HrwTarget(bucketName, newName, smap)

	for i := 0; baseNameHrw == newNameHrw; i++ {
		newName = newNamePrefix + strconv.Itoa(i)
		newNameHrw, _ = cluster.HrwTarget(bucketName, newName, smap)
	}
	return newName
}

// copyRandWithHash reads data from random source and writes it to a writer while
// optionally computing xxhash
// See related: memsys_test.copyRand
func copyRandWithHash(w io.Writer, size int64, withHash bool, rnd *rand.Rand) (string, error) {
	var (
		rem   = size
		shash string
		h     *xxhash.XXHash64
	)
	buf, s := Mem2.AllocFromSlab2(cmn.MiB)
	blkSize := int64(len(buf))
	defer s.Free(buf)

	if withHash {
		h = xxhash.New64()
	}
	for i := int64(0); i <= size/blkSize; i++ {
		n := int(cmn.MinI64(blkSize, rem))
		rnd.Read(buf[:n])
		m, err := w.Write(buf[:n])
		if err != nil {
			return "", err
		}

		if withHash {
			h.Write(buf[:m])
		}
		cmn.Assert(m == n)
		rem -= int64(m)
	}
	if withHash {
		shash = cmn.HashToStr(h)
	}
	return shash, nil
}
