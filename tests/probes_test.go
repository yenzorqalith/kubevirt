package tests_test

import (
	"flag"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v12 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/tests"
)

var _ = Describe("Probes", func() {
	flag.Parse()

	virtClient, err := kubecli.GetKubevirtClient()
	tests.PanicOnError(err)

	BeforeEach(func() {
		tests.BeforeTestCleanup()
	})

	Context("for readiness", func() {

		tcpProbe := &v12.Probe{
			PeriodSeconds:       5,
			InitialDelaySeconds: 5,
			Handler: v12.Handler{

				TCPSocket: &v1.TCPSocketAction{
					Port: intstr.Parse("1500"),
				},
			},
		}

		httpProbe := &v12.Probe{
			PeriodSeconds:       5,
			InitialDelaySeconds: 5,
			Handler: v12.Handler{

				HTTPGet: &v1.HTTPGetAction{
					Port: intstr.Parse("1500"),
				},
			},
		}
		table.DescribeTable("should succeed", func(readinessProbe *v12.Probe, serverStarter func(vmi *v12.VirtualMachineInstance, port int)) {
			By("Specifying a VMI with a readiness probe")
			vmi := tests.NewRandomVMIWithEphemeralDiskAndUserdata(tests.ContainerDiskFor(tests.ContainerDiskCirros), "#!/bin/bash\necho 'hello'\n")
			vmi.Spec.ReadinessProbe = readinessProbe
			vmi, err = virtClient.VirtualMachineInstance(tests.NamespaceTestDefault).Create(vmi)
			Expect(err).ToNot(HaveOccurred())
			tests.WaitForSuccessfulVMIStart(vmi)

			Expect(podReady(tests.GetRunningPodByVirtualMachineInstance(vmi, tests.NamespaceTestDefault))).To(Equal(v1.ConditionFalse))

			By("Starting the server inside the VMI")
			serverStarter(vmi, 1500)

			By("Checking that the VMI will be marked as ready to receive traffic")
			Eventually(func() v1.ConditionStatus {
				pod := tests.GetRunningPodByVirtualMachineInstance(vmi, tests.NamespaceTestDefault)
				return podReady(pod)
			}, 30, 1).Should(Equal(v1.ConditionTrue))
		},
			table.Entry("with working TCP probe and tcp server", tcpProbe, tests.StartTCPServer),
			table.Entry("with working HTTP probe and http server", httpProbe, tests.StartHTTPServer),
		)

		table.DescribeTable("should fail", func(readinessProbe *v12.Probe) {
			By("Specifying a VMI with a readiness probe")
			vmi := tests.NewRandomVMIWithEphemeralDiskAndUserdata(tests.ContainerDiskFor(tests.ContainerDiskCirros), "#!/bin/bash\necho 'hello'\n")
			vmi.Spec.ReadinessProbe = readinessProbe
			vmi, err = virtClient.VirtualMachineInstance(tests.NamespaceTestDefault).Create(vmi)
			Expect(err).ToNot(HaveOccurred())
			tests.WaitForSuccessfulVMIStart(vmi)

			Expect(podReady(tests.GetRunningPodByVirtualMachineInstance(vmi, tests.NamespaceTestDefault))).To(Equal(v1.ConditionFalse))

			By("Checking that the VMI will consistently stay in a not-ready state")
			Consistently(func() v1.ConditionStatus {
				pod := tests.GetRunningPodByVirtualMachineInstance(vmi, tests.NamespaceTestDefault)
				return podReady(pod)
			}, 30, 1).Should(Equal(v1.ConditionFalse))
		},
			table.Entry("with working TCP probe and no running server", tcpProbe),
			table.Entry("with working HTTP probe and no running server", httpProbe),
		)
	})
})

func podReady(pod *v1.Pod) v1.ConditionStatus {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == v1.PodReady {
			return cond.Status
		}
	}
	return v1.ConditionFalse
}
