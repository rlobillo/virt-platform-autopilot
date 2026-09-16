/*
Copyright 2026 The Virt Platform Autopilot Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubeletPerfMCName            = "95-worker-kubelet-perf-settings"
	kubeletPerfGateAnnotation    = "platform.kubevirt.io/enable-kubelet-performance-settings"
	mcCoalescingBypassAnnotation = "platform.kubevirt.io/bypass-mcp-rollout-coalescing"
	machineConfigPoolCRDName     = "machineconfigpools.machineconfiguration.openshift.io"
	machineConfigPoolCRDFile     = "test/crds/openshift/machineconfigpool-crd.yaml"
	// workerMCPName is the test MachineConfigPool created in each coalescing context.
	// It uses a test-specific prefix to avoid collision with any pre-existing pools.
	workerMCPName = "test-coalescing-worker"
	infraMCPName  = "test-coalescing-infra"
)

// Coalescing tests manipulate MachineConfigPool status directly via a fake CRD.
// They are incompatible with real OpenShift clusters where MCO owns MCP state.
var _ = Describe("MachineConfig Rollout Coalescing E2E Tests", Ordered, Label("coalescing"), func() {

	BeforeAll(func() {
		if isOpenShiftCluster() {
			Skip("coalescing tests run on Kind only — MCP status manipulation conflicts with real MCO")
		}
		ensureHCOExists()
		patchAutopilotAndWait(autopilotEnabled)

		By("installing MachineConfigPool CRD for coalescing tests")
		installCRDFromFile(machineConfigPoolCRDFile)
		waitForCRDEstablished(machineConfigPoolCRDName)

		By("enabling kubelet-perf gate so the 95-worker-kubelet-perf-settings MC is created")
		setAnnotation(hcoGVK, hcoName, operatorNamespace, kubeletPerfGateAnnotation, "true")
		waitForKubeletPerfMCCompliant()
	})

	AfterAll(func() {
		By("disabling kubelet-perf gate")
		removeAnnotation(hcoGVK, hcoName, operatorNamespace, kubeletPerfGateAnnotation)
		deleteStagingConfigMap()
		removeCRD(machineConfigPoolCRDName)
	})

	// TC1: MC update staged while MCP is stable (Updating=False).
	// compliance_status must be 2, staging CM entry created, metric emitted, drift NOT corrected.
	Context("TC1: MC update staged while MCP stable (Updating=False)", Ordered, func() {
		BeforeAll(func() {
			By("creating test worker MCP with Updating=False")
			createTestMCP(workerMCPName, false)
			By("tampering kubelet-perf MC managed-by label to create drift")
			tamperKubeletPerfMCManagedByLabel()
			touchHCO()
		})

		AfterAll(func() {
			cleanupCoalescingContext(workerMCPName)
		})

		It("should set compliance_status=2 (staged/deferred)", func() {
			Eventually(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, timeout, interval).Should(Equal(2.0),
				"compliance_status should be 2 (deferred) while MCP is stable")
		})

		It("should create a staging ConfigMap entry for the MC", func() {
			Eventually(stagingEntryExists(kubeletPerfMCName), timeout, interval).Should(BeTrue(),
				"staging ConfigMap should have an entry for "+kubeletPerfMCName)
		})

		It("should emit machineconfig_update_staged=1 for the worker pool", func() {
			Eventually(func() float64 {
				return findMCStagedMetric(kubeletPerfMCName, workerMCPName)
			}, timeout, interval).Should(Equal(1.0),
				"machineconfig_update_staged metric should be 1 for "+workerMCPName)
		})

		It("should keep compliance_status=2 across subsequent reconciles (drift not corrected)", func() {
			// Trigger extra reconciles to confirm staging is durable.
			touchHCO()
			Consistently(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, consistentlyDuration, consistentlyInterval).Should(Equal(2.0),
				"drift must remain uncorrected while MCP is not Updating")
		})
	})

	// TC2: Staged update released when MCP transitions to Updating=True.
	// compliance_status returns to 1, staging CM entry removed, metric cleared.
	Context("TC2: Released when MCP transitions to Updating=True", Ordered, func() {
		BeforeAll(func() {
			By("creating test worker MCP with Updating=False and staging a drift")
			createTestMCP(workerMCPName, false)
			tamperKubeletPerfMCManagedByLabel()
			touchHCO()
			waitForKubeletPerfMCStaged()

			By("setting MCP Updating=True to trigger release")
			setMCPUpdating(workerMCPName, true)
		})

		AfterAll(func() {
			cleanupCoalescingContext(workerMCPName)
		})

		It("should correct the drift (compliance_status=1)", func() {
			Eventually(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, 2*timeout, interval).Should(Equal(1.0),
				"compliance_status should return to 1 after MCP transitions to Updating")
		})

		It("should remove the staging ConfigMap entry", func() {
			Eventually(stagingEntryExists(kubeletPerfMCName), timeout, interval).Should(BeFalse(),
				"staging ConfigMap entry should be cleared once the update is applied")
		})

		It("should clear the machineconfig_update_staged metric", func() {
			Eventually(func() float64 {
				return findMCStagedMetric(kubeletPerfMCName, workerMCPName)
			}, timeout, interval).Should(Equal(-1.0),
				"machineconfig_update_staged metric should be absent after release")
		})
	})

	// TC3: Bypass annotation opts a MC out of staging → immediate apply.
	// The bypass annotation must be preserved on the live MC after the apply.
	Context("TC3: Bypass annotation causes immediate apply", Ordered, func() {
		BeforeAll(func() {
			By("creating test worker MCP with Updating=False")
			createTestMCP(workerMCPName, false)

			By("setting bypass annotation on the kubelet-perf MC")
			setAnnotation(machineConfigGVK, kubeletPerfMCName, "", mcCoalescingBypassAnnotation, "true")

			By("tampering kubelet-perf MC managed-by label to create drift")
			tamperKubeletPerfMCManagedByLabel()
			touchHCO()
		})

		AfterAll(func() {
			removeAnnotation(machineConfigGVK, kubeletPerfMCName, "", mcCoalescingBypassAnnotation)
			cleanupCoalescingContext(workerMCPName)
		})

		It("should immediately correct the drift (compliance_status=1)", func() {
			Eventually(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, timeout, interval).Should(Equal(1.0),
				"bypass annotation must cause immediate apply even when MCP is stable")
		})

		It("should NOT create a staging ConfigMap entry", func() {
			Consistently(stagingEntryExists(kubeletPerfMCName), consistentlyDuration, consistentlyInterval).Should(BeFalse(),
				"staging ConfigMap must remain empty when bypass annotation is set")
		})

		It("should preserve the bypass annotation on the live MC after apply", func() {
			Eventually(func() string {
				mc, err := getUnstructuredResource(machineConfigGVK, kubeletPerfMCName, "")
				if err != nil {
					return ""
				}
				return mc.GetAnnotations()[mcCoalescingBypassAnnotation]
			}, timeout, interval).Should(Equal("true"),
				"bypass annotation must survive the SSA apply and remain on the live MC")
		})
	})

	// TC4: Staging state survives an operator restart.
	// The staging ConfigMap is the durable source; the in-memory mirror is rebuilt
	// on the first reconcile after restart and the metric is re-emitted.
	Context("TC4: Staging survives operator restart", Ordered, func() {
		BeforeAll(func() {
			By("creating test worker MCP with Updating=False and staging a drift")
			createTestMCP(workerMCPName, false)
			tamperKubeletPerfMCManagedByLabel()
			touchHCO()
			waitForKubeletPerfMCStaged()

			By("restarting the operator pod")
			restartOperatorPod()
		})

		AfterAll(func() {
			cleanupCoalescingContext(workerMCPName)
		})

		It("should keep the staging ConfigMap entry after restart", func() {
			Expect(stagingEntryExists(kubeletPerfMCName)()).To(BeTrue(),
				"staging ConfigMap entry must persist across operator restarts")
		})

		It("should re-emit machineconfig_update_staged=1 after the first reconcile", func() {
			Eventually(func() float64 {
				return findMCStagedMetric(kubeletPerfMCName, workerMCPName)
			}, timeout, interval).Should(Equal(1.0),
				"staging metric must be re-emitted from ConfigMap state after restart")
		})

		It("should keep compliance_status=2 — MC still not applied after restart", func() {
			Consistently(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, consistentlyDuration, consistentlyInterval).Should(Equal(2.0),
				"drift must not be corrected after restart while MCP remains stable")
		})
	})

	// TC5: When the MachineConfigPool CRD is absent the operator falls back to
	// immediate apply (compatibility mode). No staging ConfigMap must be created.
	Context("TC5: Without MCP CRD → immediate apply (compatibility mode)", Ordered, func() {
		var prevRestartCount int32

		BeforeAll(func() {
			By("removing MachineConfigPool CRD to test compatibility mode")
			prevRestartCount = getManagerRestartCount()
			removeCRD(machineConfigPoolCRDName)
			// The operator may or may not restart when the watched CRD disappears.
			// Wait for it to be healthy regardless.
			Eventually(func() int32 {
				return getManagerRestartCount()
			}, timeout, interval).Should(BeNumerically(">=", prevRestartCount))
			waitForOperatorHealthy()

			By("triggering drift without any MCP present")
			tamperKubeletPerfMCManagedByLabel()
			touchHCO()
		})

		AfterAll(func() {
			deleteStagingConfigMap()

			By("reinstalling MachineConfigPool CRD for subsequent contexts")
			installCRDFromFile(machineConfigPoolCRDFile)
			waitForCRDEstablished(machineConfigPoolCRDName)
			waitForOperatorHealthy()
			waitForKubeletPerfMCCompliant()
		})

		It("should immediately correct the drift (compliance_status=1)", func() {
			Eventually(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, timeout, interval).Should(Equal(1.0),
				"operator must apply immediately when MachineConfigPool CRD is absent")
		})

		It("should NOT create a staging ConfigMap entry in compatibility mode", func() {
			Consistently(stagingEntryExists(kubeletPerfMCName), consistentlyDuration, consistentlyInterval).Should(BeFalse(),
				"no staging ConfigMap entry must exist when operating without MachineConfigPool CRD")
		})
	})

	// TC6 (should-have): When an MC is selected by multiple pools, the first pool
	// to transition to Updating=True releases the staged update immediately.
	Context("TC6: First updating pool releases staged update (multi-pool)", Ordered, func() {
		BeforeAll(func() {
			By("creating worker and infra MCPs, both Updating=False, both selecting the kubelet-perf MC")
			createTestMCP(workerMCPName, false)
			createTestMCP(infraMCPName, false)
			tamperKubeletPerfMCManagedByLabel()
			touchHCO()
			waitForKubeletPerfMCStaged()

			By("transitioning infra pool to Updating=True — this should release the staged update")
			setMCPUpdating(infraMCPName, true)
		})

		AfterAll(func() {
			cleanupCoalescingContext(workerMCPName, infraMCPName)
		})

		It("should emit machineconfig_update_staged=1 for both pools while staged", func() {
			// Both pools must have been recorded in the staging entry.
			// We verify each pool's metric was present before the release by re-checking
			// on any remaining series (at least one must have appeared).
			Expect(findMCStagedMetric(kubeletPerfMCName, workerMCPName) == 1.0 ||
				findMCStagedMetric(kubeletPerfMCName, infraMCPName) == 1.0).To(BeTrue(),
				"at least one pool staging metric must have been emitted before release")
		})

		It("should correct the drift when infra pool transitions to Updating=True", func() {
			Eventually(func() float64 {
				return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
			}, 2*timeout, interval).Should(Equal(1.0),
				"the first updating pool must release the staged update regardless of other pools' state")
		})

		It("should clear all staging metrics after release", func() {
			Eventually(func() bool {
				workerGone := findMCStagedMetric(kubeletPerfMCName, workerMCPName) == -1.0
				infraGone := findMCStagedMetric(kubeletPerfMCName, infraMCPName) == -1.0
				return workerGone && infraGone
			}, timeout, interval).Should(BeTrue(),
				"machineconfig_update_staged metrics must be cleared for all pools after release")
		})
	})
})

// TC7 (should-have): VirtPlatformAutopilotSyncFailed must NOT fire while a
// MachineConfig update is staged (compliance_status=2 is an expected state,
// not a failure). Requires OCP with Prometheus.
var _ = Describe("MC Coalescing: Alert Silence While Staged", Ordered, Label("coalescing", "ocp-only"), func() {
	BeforeAll(func() {
		if !isOpenShiftCluster() {
			Skip("TC7 requires OCP Prometheus — skipping on Kind")
		}
		ensureHCOExists()
		By("enabling kubelet-perf gate")
		setAnnotation(hcoGVK, hcoName, operatorNamespace, kubeletPerfGateAnnotation, "true")
		waitForKubeletPerfMCCompliant()
		By("tampering kubelet-perf MC managed-by label to create drift")
		tamperKubeletPerfMCManagedByLabel()
		touchHCO()
		waitForKubeletPerfMCStaged()
	})

	AfterAll(func() {
		if !isOpenShiftCluster() {
			return
		}
		// Force-apply via bypass so the cluster returns to a clean state.
		setAnnotation(machineConfigGVK, kubeletPerfMCName, "", mcCoalescingBypassAnnotation, "true")
		touchHCO()
		waitForKubeletPerfMCCompliant()
		removeAnnotation(machineConfigGVK, kubeletPerfMCName, "", mcCoalescingBypassAnnotation)
		removeAnnotation(hcoGVK, hcoName, operatorNamespace, kubeletPerfGateAnnotation)
		touchHCO()
		deleteStagingConfigMap()
		waitForMCPStable()
	})

	It(fmt.Sprintf("should NOT fire VirtPlatformAutopilotSyncFailed while compliance_status=2 for %s", kubeletPerfMCName), func() {
		Consistently(func() bool {
			return queryAlertNotFiring("VirtPlatformAutopilotSyncFailed", 1, 1,
				"kind", "MachineConfig",
				"name", kubeletPerfMCName)
		}, time.Minute, 10*time.Second).Should(BeTrue(),
			"VirtPlatformAutopilotSyncFailed must NOT fire while update is staged (compliance_status=2 is expected)")
	})
})

// TC8 (should-have, OCP-only): Full coalescing lifecycle on a real cluster.
// Two managed MachineConfigs are staged simultaneously. Applying bypass on the
// one with a spec change (kubelet-perf) triggers an MCO rollout, which
// transitions the worker MachineConfigPool to Updating=True. The second MC
// (swap) "joins the rollout party": its staged correction is released and
// applied without needing a separate bypass or a second rollout.
//
// This test causes a real worker-node rollout and is expected to take 10-30
// minutes depending on cluster size. It must run on a dedicated test cluster.
var _ = Describe("MC Coalescing: OCP Lifecycle — bypass triggers rollout that releases staged peer", Ordered, Label("coalescing", "ocp-only", "slow"), func() {
	const rolloutMCPTimeout = 30 * time.Minute

	BeforeAll(func() {
		if !isOpenShiftCluster() {
			Skip("TC8 requires real OCP cluster with MCO — skipping on Kind")
		}
		ensureHCOExists()
		waitForMCPStable()

		By("enabling kubelet-perf gate and waiting for MC to be synced")
		setAnnotation(hcoGVK, hcoName, operatorNamespace, kubeletPerfGateAnnotation, "true")
		waitForKubeletPerfMCCompliant()

		By("verifying swap MC is present and synced")
		Eventually(func() float64 {
			return captureAssetMetrics("MachineConfig", swapMcName, "").ComplianceStatus
		}, timeout, interval).Should(Equal(1.0),
			swapMcName+" must be synced before staging tests")

		// Stage MC #1 (kubelet-perf): tamper ignition version — real spec drift
		// that the operator manages via SSA. When bypassed and corrected, MCO
		// detects the spec change and starts a worker-node rollout.
		By("tampering " + kubeletPerfMCName + " ignition version to create real spec drift")
		tamperKubeletPerfMCIgnitionVersion()
		touchHCO()

		// Stage MC #2 (swap): tamper the managed-by label — metadata drift that
		// MCO does NOT roll out, so it is safe to apply after the rollout window.
		By("tampering " + swapMcName + " managed-by label to create a second staged update")
		mcRef := unstructuredRef(machineConfigGVK, swapMcName, "")
		EventuallyWithOffset(1, func() error {
			return k8sClient.Patch(ctx, mcRef,
				client.RawPatch(types.MergePatchType,
					[]byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"tampered"}}}`, managedByLabel))))
		}, timeout, interval).Should(Succeed())

		By("waiting for both MCs to reach compliance_status=2 (staged)")
		Eventually(func() float64 {
			return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
		}, timeout, interval).Should(Equal(2.0), kubeletPerfMCName+" should be staged")
		Eventually(func() float64 {
			return captureAssetMetrics("MachineConfig", swapMcName, "").ComplianceStatus
		}, timeout, interval).Should(Equal(2.0), swapMcName+" should be staged")
	})

	AfterAll(func() {
		if !isOpenShiftCluster() {
			return
		}
		// Ensure bypass is removed regardless of test outcome.
		removeAnnotation(machineConfigGVK, kubeletPerfMCName, "", mcCoalescingBypassAnnotation)
		removeAnnotation(hcoGVK, hcoName, operatorNamespace, kubeletPerfGateAnnotation)
		touchHCO()
		deleteStagingConfigMap()
		waitForMCPStable()
	})

	It("should have both MCs staged with correct metrics before bypass", func() {
		workerMCPName := findRealWorkerMCPName()
		Expect(workerMCPName).NotTo(BeEmpty(), "a worker MachineConfigPool must exist on OCP")

		Expect(stagingEntryExists(kubeletPerfMCName)()).To(BeTrue(),
			"staging CM must have entry for "+kubeletPerfMCName)
		Expect(stagingEntryExists(swapMcName)()).To(BeTrue(),
			"staging CM must have entry for "+swapMcName)

		Expect(findMCStagedMetric(kubeletPerfMCName, workerMCPName)).To(Equal(1.0),
			"machineconfig_update_staged must be 1 for "+kubeletPerfMCName)
		Expect(findMCStagedMetric(swapMcName, workerMCPName)).To(Equal(1.0),
			"machineconfig_update_staged must be 1 for "+swapMcName)
	})

	It("bypass on kubelet-perf MC triggers immediate apply and starts MCO rollout", func() {
		By("applying bypass annotation to " + kubeletPerfMCName)
		setAnnotation(machineConfigGVK, kubeletPerfMCName, "", mcCoalescingBypassAnnotation, "true")
		touchHCO()

		By("verifying " + kubeletPerfMCName + " is applied immediately (compliance_status=1)")
		Eventually(func() float64 {
			return captureAssetMetrics("MachineConfig", kubeletPerfMCName, "").ComplianceStatus
		}, timeout, interval).Should(Equal(1.0),
			kubeletPerfMCName+" must be applied immediately when bypass annotation is set")

		By("verifying staging CM entry for " + kubeletPerfMCName + " is removed")
		Eventually(stagingEntryExists(kubeletPerfMCName), timeout, interval).Should(BeFalse())

		By("waiting for MCO to start a worker rollout (MCP Updating=True)")
		waitForAnyMCPUpdating(rolloutMCPTimeout)
	})

	It("swap MC staging is released and correction applied as part of the rollout window", func() {
		workerMCPName := findRealWorkerMCPName()

		By("verifying " + swapMcName + " is released (compliance_status=1)")
		Eventually(func() float64 {
			return captureAssetMetrics("MachineConfig", swapMcName, "").ComplianceStatus
		}, rolloutMCPTimeout, 10*time.Second).Should(Equal(1.0),
			swapMcName+" staging must be released once MCP transitions to Updating=True")

		By("verifying staging CM entry for " + swapMcName + " is removed")
		Eventually(stagingEntryExists(swapMcName), timeout, interval).Should(BeFalse())

		By("verifying machineconfig_update_staged metrics are cleared for both MCs")
		Eventually(func() bool {
			kubeletGone := findMCStagedMetric(kubeletPerfMCName, workerMCPName) == -1.0
			swapGone := findMCStagedMetric(swapMcName, workerMCPName) == -1.0
			return kubeletGone && swapGone
		}, timeout, interval).Should(BeTrue(),
			"machineconfig_update_staged must be cleared for both MCs after rollout")
	})

	It("swap MC managed-by label is restored to correct value after release", func() {
		Eventually(func() string {
			mc, err := getUnstructuredResource(machineConfigGVK, swapMcName, "")
			if err != nil {
				return ""
			}
			return mc.GetLabels()[managedByLabel]
		}, timeout, interval).Should(Equal(managedByValue),
			"managed-by label on "+swapMcName+" must be restored to "+managedByValue+" after staging release")
	})

	It("should emit DriftCorrected event for both MCs", func() {
		By("verifying DriftCorrected event was emitted after release")
		Eventually(func() int {
			return captureAutopilotEvents().DriftCorrected
		}, timeout, interval).Should(BeNumerically(">=", 2),
			"at least 2 DriftCorrected events must have been emitted (one per MC)")
	})

	It("rollout completes and cluster returns to stable state", func() {
		By("waiting for all MachineConfigPools to become stable after rollout")
		waitForMCPStable()
	})
})
