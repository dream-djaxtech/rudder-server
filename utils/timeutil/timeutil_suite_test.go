package timeutil_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestTimeutil(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Timeutil Suite")
}
