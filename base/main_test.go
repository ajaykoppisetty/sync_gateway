package base

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// base tests require GSI
	TestBucketPool = NewTestBucketPool(BucketFlushReadier, NoopBucketInitFunc)

	status := m.Run()

	TestBucketPool.Close()

	os.Exit(status)
}
