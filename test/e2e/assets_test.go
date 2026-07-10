package e2e

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime/schema"

	pkgassets "github.com/kubevirt/virt-platform-autopilot/pkg/assets"
)

// UserOverrideFieldSpec identifies an operator-controlled field on an asset
// that the E2E tests can safely tamper with to verify patch, ignore-fields,
// and unmanaged-mode behavior.
//
// Only JSONPointer and Values are stored; all other formats (FieldPath,
// merge-patch JSON, RFC 6902 patch document) are derived via methods.
type UserOverrideFieldSpec struct {
	// JSONPointer is the RFC 6901 pointer to the field (e.g., "/metadata/labels/app").
	JSONPointer string
	// Values holds two distinct tamper values for the field, compared via
	// fmt.Sprintf("%v") to support strings, int64, and float64.
	Values [2]string
}

// FieldPath returns the unstructured nested path for reading the field,
// derived from JSONPointer with RFC 6901 unescaping (~1→/, ~0→~).
func (o UserOverrideFieldSpec) FieldPath() []string {
	parts := strings.Split(o.JSONPointer, "/")[1:]
	for i, p := range parts {
		p = strings.ReplaceAll(p, "~1", "/")
		p = strings.ReplaceAll(p, "~0", "~")
		parts[i] = p
	}
	return parts
}

func (o UserOverrideFieldSpec) jsonEncodeValue(idx int) string {
	s := o.Values[idx]
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// MergePatch returns a Kubernetes merge-patch JSON that sets the field to Values[idx].
func (o UserOverrideFieldSpec) MergePatch(idx int) string {
	path := o.FieldPath()
	result := o.jsonEncodeValue(idx)
	for i := len(path) - 1; i >= 0; i-- {
		result = fmt.Sprintf(`{%q:%s}`, path[i], result)
	}
	return result
}

// PatchDoc returns an RFC 6902 JSON Patch document that replaces the field with Values[0].
func (o UserOverrideFieldSpec) PatchDoc() string {
	return fmt.Sprintf(`[{"op":"replace","path":"%s","value":%s}]`, o.JSONPointer, o.jsonEncodeValue(0))
}

type testAsset struct {
	MetadataName  string // matches AssetMetadata.Name in metadata.yaml
	GVK           schema.GroupVersionKind
	Plural        string
	Name          string
	Namespace     string
	GateCRD       string
	ClusterScoped bool
	Sensitive     bool
	Override      UserOverrideFieldSpec
}

func (a testAsset) webhookName() string {
	return fmt.Sprintf("autopilot-e2e-block-%s", a.Plural)
}

// sensitiveKinds mirrors the blocklist in pkg/overrides/validation.go.
// The operator rejects JSON patches on these kinds for security reasons.
// initAssets uses this map to set testAsset.Sensitive automatically so
// new assets of a sensitive kind are flagged without manual annotation.
var sensitiveKinds = map[string]bool{
	"MachineConfig":                  true,
	"KubeletConfig":                  true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"Role":                           true,
	"RoleBinding":                    true,
	"ServiceAccount":                 true,
	"PodSecurityPolicy":              true,
	"SecurityContextConstraints":     true,
	"ValidatingWebhookConfiguration": true,
	"MutatingWebhookConfiguration":   true,
}

// assetsUnderTest lists all reconcile_order=1 assets from metadata.yaml.
// The coverage test validates completeness against the asset catalog.
// Used by anti-thrashing, alert, and other E2E test suites.
// Sensitive is derived automatically from sensitiveKinds.
var assetsUnderTest = initAssets([]testAsset{
	// Phase 1: Observability
	{
		MetadataName:  "metrics-service",
		GVK:           schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"},
		Plural:        "services",
		Name:          "virt-platform-autopilot-metrics",
		Namespace:     "openshift-cnv",
		ClusterScoped: false,
		Override: UserOverrideFieldSpec{
			JSONPointer: "/metadata/labels/app",
			Values:      [2]string{"e2e-tampered", "e2e-modified"},
		},
	},
	{
		MetadataName:  "metrics-servicemonitor",
		GVK:           schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
		Plural:        "servicemonitors",
		Name:          "virt-platform-autopilot-metrics",
		Namespace:     "openshift-cnv",
		GateCRD:       "servicemonitors.monitoring.coreos.com",
		ClusterScoped: false,
		Override: UserOverrideFieldSpec{
			JSONPointer: "/metadata/labels/app",
			Values:      [2]string{"e2e-tampered", "e2e-modified"},
		},
	},
	{
		MetadataName:  "prometheus-alerts",
		GVK:           schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"},
		Plural:        "prometheusrules",
		Name:          "virt-platform-autopilot-alerts",
		Namespace:     "openshift-cnv",
		GateCRD:       "prometheusrules.monitoring.coreos.com",
		ClusterScoped: false,
		Override: UserOverrideFieldSpec{
			JSONPointer: "/metadata/labels/role",
			Values:      [2]string{"e2e-tampered", "e2e-modified"},
		},
	},

	// Phase 1: MachineConfig
	{
		MetadataName:  "swap-enable",
		GVK:           schema.GroupVersionKind{Group: "machineconfiguration.openshift.io", Version: "v1", Kind: "MachineConfig"},
		Plural:        "machineconfigs",
		Name:          "90-worker-swap-online",
		ClusterScoped: true,
	},
	{
		MetadataName:  "psi-enable",
		GVK:           schema.GroupVersionKind{Group: "machineconfiguration.openshift.io", Version: "v1", Kind: "MachineConfig"},
		Plural:        "machineconfigs",
		Name:          "99-openshift-machineconfig-worker-psi-karg",
		GateCRD:       "kubedeschedulers.operator.openshift.io",
		ClusterScoped: true,
	},

	// Phase 1: KubeletConfig
	{
		MetadataName:  "kubelet-perf-settings",
		GVK:           schema.GroupVersionKind{Group: "machineconfiguration.openshift.io", Version: "v1", Kind: "KubeletConfig"},
		Plural:        "kubeletconfigs",
		Name:          "virt-perf-settings",
		GateCRD:       "kubeletconfigs.machineconfiguration.openshift.io",
		ClusterScoped: true,
	},

	// Phase 1: Descheduler
	{
		MetadataName:  "descheduler-loadaware",
		GVK:           schema.GroupVersionKind{Group: "operator.openshift.io", Version: "v1", Kind: "KubeDescheduler"},
		Plural:        "kubedeschedulers",
		Name:          "cluster",
		Namespace:     "openshift-kube-descheduler-operator",
		GateCRD:       "kubedeschedulers.operator.openshift.io",
		ClusterScoped: false,
		Override: UserOverrideFieldSpec{
			JSONPointer: "/spec/deschedulingIntervalSeconds",
			Values:      [2]string{"120", "180"},
		},
	},
})

func initAssets(assets []testAsset) []testAsset {
	for i := range assets {
		assets[i].Sensitive = sensitiveKinds[assets[i].GVK.Kind]
	}
	return assets
}

var _ = Describe("Asset catalog coverage", func() {
	It("should report reconcile_order=1 assets not covered by assetsUnderTest", func() {
		registry, err := pkgassets.NewRegistry(pkgassets.NewLoader())
		Expect(err).NotTo(HaveOccurred())

		catalogNames := map[string]bool{}
		for _, a := range registry.ListAssets(nil) {
			if a.ReconcileOrder == 1 {
				catalogNames[a.Name] = true
			}
		}

		testNames := map[string]bool{}
		for _, a := range assetsUnderTest {
			Expect(a.MetadataName).NotTo(BeEmpty(),
				fmt.Sprintf("testAsset %s/%s is missing MetadataName", a.GVK.Kind, a.Name))
			testNames[a.MetadataName] = true
		}

		var missingFromTests []string
		for name := range catalogNames {
			if !testNames[name] {
				missingFromTests = append(missingFromTests, name)
			}
		}
		sort.Strings(missingFromTests)

		var extraInTests []string
		for name := range testNames {
			if !catalogNames[name] {
				extraInTests = append(extraInTests, name)
			}
		}
		sort.Strings(extraInTests)

		if len(missingFromTests) > 0 {
			AddReportEntry("WARNING: reconcile_order=1 assets not covered by e2e tests",
				fmt.Sprintf("%v\nConsider adding testAsset entries to assetsUnderTest in assets_test.go", missingFromTests))
		}
		Expect(extraInTests).To(BeEmpty(),
			fmt.Sprintf("assets in assetsUnderTest but not reconcile_order=1 in metadata.yaml: %v\n"+
				"ACTION REQUIRED: remove the stale testAsset entries from assetsUnderTest, "+
				"or verify the asset's reconcile_order in assets/active/metadata.yaml", extraInTests))
	})
})
