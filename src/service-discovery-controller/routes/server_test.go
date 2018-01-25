package routes_test

import (
	"test-helpers"

	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"service-discovery-controller/config"
	. "service-discovery-controller/routes"
	"service-discovery-controller/routes/fakes"
	"strconv"
	"time"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagertest"

	"code.cloudfoundry.org/cf-networking-helpers/testsupport/ports"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/tedsuo/ifrit"
)

var _ = Describe("Server", func() {
	var (
		addressTable       *fakes.AddressTable
		dnsRequestRecorder *fakes.DNSRequestRecorder
		metricsSender      *fakes.MetricsSender
		clientCert         tls.Certificate
		caFile             string
		serverCert         string
		serverKey          string
		serverProc         ifrit.Process
		testLogger         *lagertest.TestLogger
		server             *Server
		port               int
	)

	BeforeEach(func() {
		caFile, serverCert, serverKey, clientCert = testhelpers.GenerateCaAndMutualTlsCerts()

		port = ports.PickAPort()

		testLogger = lagertest.NewTestLogger("test")
		config := &config.Config{
			Port:       strconv.Itoa(port),
			Address:    "127.0.0.1",
			CACert:     caFile,
			ServerCert: serverCert,
			ServerKey:  serverKey,
		}
		addressTable = &fakes.AddressTable{}
		dnsRequestRecorder = &fakes.DNSRequestRecorder{}
		metricsSender = &fakes.MetricsSender{}
		server = NewServer(addressTable, config, dnsRequestRecorder, metricsSender, testLogger)
	})

	Context("when the lookup succeeds", func() {
		var respBody string

		BeforeEach(func() {
			serverProc = ifrit.Invoke(server)
			addressTable.LookupReturns([]string{"192.168.0.2"})
			addressTable.IsWarmReturns(true)

			client := testhelpers.NewClient(testhelpers.CertPool(caFile), clientCert)
			resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/v1/registration/app-id.internal.local.", port))
			Expect(err).ToNot(HaveOccurred())
			respBodyBytes, err := ioutil.ReadAll(resp.Body)
			Expect(err).ToNot(HaveOccurred())
			respBody = string(respBodyBytes)
		})

		AfterEach(func() {
			serverProc.Signal(os.Interrupt)
			Eventually(serverProc.Wait()).Should(Receive())
		})

		It("should return addresses for a give hostname", func() {
			Expect(string(respBody)).To(MatchJSON(`{
				"env": "",
				"hosts": [
				{
					"ip_address": "192.168.0.2",
					"last_check_in": "",
					"port": 0,
					"revision": "",
					"service": "",
					"service_repo_name": "",
					"tags": {}
				}],
				"service": ""
			}`))
		})

		It("looks up the given host name in the address table", func() {
			Expect(addressTable.LookupCallCount()).To(Equal(1))
			Expect(addressTable.LookupArgsForCall(0)).To(Equal("app-id.internal.local."))
		})

		It("invokes the dns request recorder", func() {
			Expect(dnsRequestRecorder.RecordRequestCallCount()).To(Equal(1))
		})

		It("invokes our metrics sender", func() {
			Expect(metricsSender.SendDurationCallCount()).To(Equal(1))
			name, time := metricsSender.SendDurationArgsForCall(0)
			Expect(name).To(Equal("addressTableLookupTime"))
			Expect(time.String()).ToNot(Equal("0s"))
		})
	})

	Context("when the routes endpoint is called", func() {
		var respBody string
		BeforeEach(func() {
			serverProc = ifrit.Invoke(server)
			addressTable.GetAllAddressesReturns(map[string][]string{
				"route1": []string{"address1", "address1b"},
				"route2": []string{"address2", "address2b"},
			})

			client := testhelpers.NewClient(testhelpers.CertPool(caFile), clientCert)
			resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/routes", port))
			Expect(err).ToNot(HaveOccurred())

			respBodyBytes, err := ioutil.ReadAll(resp.Body)
			Expect(err).ToNot(HaveOccurred())
			respBody = string(respBodyBytes)
		})
		AfterEach(func() {
			serverProc.Signal(os.Interrupt)
			Eventually(serverProc.Wait()).Should(Receive())
		})
		It("returns all the addresses", func() {
			Expect(string(respBody)).To(MatchJSON(`{
				"addresses": [
				{ "hostname": "route1", "ips": ["address1", "address1b"] },
				{ "hostname": "route2", "ips": ["address2", "address2b"] }
				]
			}`))
		})
	})

	Context("when the address table is not warm", func() {
		var (
			resp *http.Response
		)
		BeforeEach(func() {
			serverProc = ifrit.Invoke(server)
			addressTable.IsWarmReturns(false)

			client := testhelpers.NewClient(testhelpers.CertPool(caFile), clientCert)
			var err error
			resp, err = client.Get(fmt.Sprintf("https://127.0.0.1:%d/v1/registration/app-id.internal.local.", port))
			Expect(err).ToNot(HaveOccurred())
		})

		It("returns an internal server error", func() {
			Expect(resp.StatusCode).To(Equal(http.StatusInternalServerError))

			respBodyBytes, err := ioutil.ReadAll(resp.Body)
			Expect(err).ToNot(HaveOccurred())
			respBody := string(respBodyBytes)
			Expect(respBody).To(ContainSubstring("address table is not warm"))
		})

		It("logs the error at debug level", func() {
			Expect(testLogger.Logs()).To(HaveLen(2))
			Expect(testLogger.Logs()[1]).To(SatisfyAll(
				LogsWith(lager.DEBUG, "test.failed-request"),
				HaveLogData(SatisfyAll(
					HaveLen(2),
					HaveKeyWithValue("serviceKey", Equal("app-id.internal.local.")),
					HaveKeyWithValue("reason", Equal("address-table-not-warm")),
				)),
			))
		})
	})

	Context("when signaled an interrupt", func() {
		It("shuts down", func() {
			serverProc = ifrit.Invoke(server)

			Eventually(testLogger.LogMessages).Should(ContainElement("test.server-started"))

			serverProc.Signal(os.Interrupt)
			Eventually(serverProc.Wait()).Should(Receive())
			Eventually(testLogger.LogMessages).Should(ContainElement("test.SDC http server exiting with signal: interrupt"))

			client := testhelpers.NewClient(testhelpers.CertPool(caFile), clientCert)
			_, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/v1/registration/app-id.internal.local.", port))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("connection refused"))
		})
	})

	Context("when it is unable to start", func() {
		var conflictingServer *http.Server

		BeforeEach(func() {
			conflictingServer = testhelpers.LaunchConflictingServer(port)
		})

		AfterEach(func() {
			conflictingServer.Close()
			serverProc.Signal(os.Interrupt)
			Eventually(serverProc.Wait()).Should(Receive())
		})

		It("logs and quits", func() {
			serverProc = ifrit.Invoke(server)
			Eventually(serverProc.Wait()).Should(Receive())
			Eventually(testLogger.LogMessages(), 5*time.Second).Should(
				ContainElement(fmt.Sprintf("test.SDC http server exiting with: listen tcp 127.0.0.1:%d: bind: address already in use", port)),
			)
		})
	})
})
