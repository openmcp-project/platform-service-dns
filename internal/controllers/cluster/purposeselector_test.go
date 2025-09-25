package cluster_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/yaml"

	dnsv1alpha1 "github.com/openmcp-project/platform-service-dns/api/dns/v1alpha1"
)

// These are tests for the purpose selector logic.
// It is implemented in the api package, but tested here to avoid testing dependencies in the api package.

// purposeSelectorTest is a helper struct for testing purpose selectors.
// It can be parsed from JSON/YAML and contains a PurposeSelector, a map of purpose sets, and a list of expected matches.
// Its Test method fails if not exactly the purpose sets whose names are in Expected match the PurposeSelector (the order doesn't matter).
type purposeSelectorTest struct {
	PurposeSelector *dnsv1alpha1.PurposeSelector `json:"purposeSelector,omitempty"`
	PurposeSets     map[string][]string          `json:"purposeSets,omitempty"`
	Expected        []string                     `json:"expected,omitempty"`
}

func (pst *purposeSelectorTest) Test() {
	matched := sets.New[string]()
	for name, purposes := range pst.PurposeSets {
		if pst.PurposeSelector.Matches(purposes) {
			matched.Insert(name)
		}
	}
	Expect(matched.UnsortedList()).To(ConsistOf(pst.Expected), "PurposeSelector did not match expected purpose sets")
}

var _ = Describe("Purpose Selector", func() {

	It("should match purpose selectors correctly", func() {
		// Read all files in testdata/purposeselector that end in '.yaml', '.yml', or '.json'.
		// Nested directories are ignored.
		entries, err := os.ReadDir(filepath.Join("testdata", "purposeselector"))
		Expect(err).ToNot(HaveOccurred())
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			ext := filepath.Ext(name)
			if ext != ".yaml" && ext != ".yml" && ext != ".json" {
				continue
			}
			// read file
			By("testing purpose selector in file " + name)
			data, err := os.ReadFile(filepath.Join("testdata", "purposeselector", name))
			Expect(err).ToNot(HaveOccurred())
			// parse file
			pst := &purposeSelectorTest{}
			Expect(yaml.Unmarshal(data, pst)).To(Succeed())
			// test purpose selector
			pst.Test()
		}
	})

})
