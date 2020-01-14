package rest

import (
	"os"
	"testing"

	"github.com/couchbase/sync_gateway/base"
	"github.com/couchbase/sync_gateway/db"
)

func TestMain(m *testing.M) {
	base.TestBucketPool = base.NewTestBucketPool(db.ViewsAndGSIBucketReadier)
	defer base.TestBucketPool.Close()

	os.Exit(m.Run())
}
