package event_schema_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestAlert(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Alert Suite")
}
