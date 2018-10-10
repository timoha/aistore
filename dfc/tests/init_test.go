/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc_test

import (
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/NVIDIA/dfcpub/dfc"
	"github.com/NVIDIA/dfcpub/dfc/tests/util"
	"github.com/NVIDIA/dfcpub/pkg/client"
)

const (
	baseDir                 = "/tmp/dfc"
	LocalDestDir            = "/tmp/dfc/dest" // client-side download destination
	LocalSrcDir             = "/tmp/dfc/src"  // client-side src directory for upload
	ColdValidStr            = "coldmd5"
	ChksumValidStr          = "chksum"
	ColdMD5str              = "coldmd5"
	DeleteDir               = "/tmp/dfc/delete"
	ChecksumWarmValidateDir = "/tmp/dfc/checksumWarmValidate"
	ChecksumWarmValidateStr = "checksumWarmValidate"
	RangeGetDir             = "/tmp/dfc/rangeGet"
	RangeGetStr             = "rangeGet"
	DeleteStr               = "delete"
	SmokeDir                = "/tmp/dfc/smoke" // smoke test dir
	SmokeStr                = "smoke"
	largefilesize           = 4                // in MB
	PhysMemSizeWarn         = uint64(7 * 1024) // MBs
	ProxyURL                = "http://localhost:8080"
)

var (
	numops                 int
	numfiles               int
	numworkers             int
	match                  = ".*"
	props                  string
	pagesize               int64
	fnlen                  int
	objlimit               int64
	prefix                 string
	abortonerr             = false
	readerType             = client.ReaderTypeSG
	prefetchPrefix         = "__bench/test-"
	prefetchRegex          = "^\\d22\\d?"
	prefetchRange          = "0:2000"
	skipdel                bool
	baseseed               = int64(1062984096)
	keepaliveSeconds       int64
	proxyChangeLatency     time.Duration // time for a cluster to stabilize after proxy changes
	startupGetSmapDelay    int64
	multiProxyTestDuration time.Duration
	clichecksum            string
	cycles                 int

	clibucket  string
	proxyURLRO string // user-defined primary proxy URL - it is read-only variable and tests mustn't change it
	usingSG    bool   // True if using SGL as reader backing memory
	usingFile  bool   // True if using file as reader backing
)

func init() {
	flag.StringVar(&proxyURLRO, "url", ProxyURL, "Proxy URL")
	flag.IntVar(&numfiles, "numfiles", 100, "Number of the files to download")
	flag.IntVar(&numworkers, "numworkers", 10, "Number of the workers")
	flag.StringVar(&match, "match", ".*", "object name regex")
	flag.StringVar(&props, "props", "", "List of object properties to return. Empty value means default set of properties")
	flag.Int64Var(&pagesize, "pagesize", 1000, "The maximum number of objects returned in one page")
	flag.Int64Var(&objlimit, "objlimit", 0, "The maximum number of objects returned in one list bucket call (0 - no limit)")
	flag.StringVar(&prefix, "prefix", "", "Object name prefix")
	flag.BoolVar(&abortonerr, "abortonerr", abortonerr, "abort on error")
	flag.StringVar(&prefetchPrefix, "prefetchprefix", prefetchPrefix, "Prefix for Prefix-Regex Prefetch")
	flag.StringVar(&prefetchRegex, "prefetchregex", prefetchRegex, "Regex for Prefix-Regex Prefetch")
	flag.StringVar(&prefetchRange, "prefetchrange", prefetchRange, "Range for Prefix-Regex Prefetch")
	flag.StringVar(&readerType, "readertype", client.ReaderTypeSG,
		fmt.Sprintf("Type of reader. {%s(default) | %s | %s | %s", client.ReaderTypeSG,
			client.ReaderTypeFile, client.ReaderTypeInMem, client.ReaderTypeRand))
	flag.BoolVar(&skipdel, "nodel", false, "Run only PUT and GET in a loop and do cleanup once at the end")
	flag.IntVar(&numops, "numops", 4, "Number of PUT/GET per worker")
	flag.IntVar(&fnlen, "fnlen", 20, "Length of randomly generated filenames")
	// When running multiple tests at the same time on different threads, ensure that
	// They are given different seeds, as the tests are completely deterministic based on
	// choice of seed, so they will interfere with each other.
	flag.Int64Var(&baseseed, "seed", baseseed, "Seed to use for random number generators")
	flag.Int64Var(&keepaliveSeconds, "keepaliveseconds", 15, "The keepalive poll time for the cluster")
	flag.Int64Var(&startupGetSmapDelay, "startupgetsmapdelay", 10, "The Startup Get Smap Delay time for proxies")
	flag.DurationVar(&multiProxyTestDuration, "duration", 3*time.Minute,
		"The length to run the Multiple Proxy Stress Test for")
	flag.StringVar(&clichecksum, "checksum", "all", "all | xxhash | coldmd5")
	flag.IntVar(&cycles, "cycles", 15, "Number of PUT cycles")
	flag.DurationVar(&proxyChangeLatency, "proxychangelatency", time.Minute*2, "Time for cluster to stabilize after a proxy change")

	flag.Parse()

	usingSG = readerType == client.ReaderTypeSG
	usingFile = readerType == client.ReaderTypeFile
	checkMemory()

	if util.DockerRunning() && proxyURLRO == ProxyURL {
		proxyURLRO = "http://172.50.0.2:8080"
	}
}

func checkMemory() {
	if readerType == client.ReaderTypeSG || readerType == client.ReaderTypeInMem {
		megabytes, _ := dfc.TotalMemory()
		if megabytes < PhysMemSizeWarn {
			fmt.Fprintf(os.Stderr, "Warning: host memory size = %dMB may be insufficient, consider use other reader type\n", megabytes)
		}
	}
}

func TestMain(m *testing.M) {
	clibucket = os.Getenv("BUCKET")
	if len(clibucket) == 0 {
		fmt.Println("Bucket name is empty.")
		os.Exit(1)
	}

	// primary proxy can change if proxy tests are run and no new cluster is re-deployed before each test
	// find out who is the current primary proxy
	url, err := client.GetPrimaryProxy(proxyURLRO)
	if err != nil {
		fmt.Printf("Failed to get primary proxy, err = %v", err)
		os.Exit(1)
	}

	proxyURLRO = url
	os.Exit(m.Run())
}
