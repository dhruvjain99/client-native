// Copyright 2019 HAProxy Technologies
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package configuration

import (
	"fmt"
	"github.com/dhruvjain99/client-native/v5/runtime"
	jsoniter "github.com/json-iterator/go"
	"strings"

	"github.com/dhruvjain99/client-native/v5/misc"
	"github.com/dhruvjain99/client-native/v5/models"
)

// ServiceGrowthTypeLinear indicates linear growth type in ScalingParams.
const ServiceGrowthTypeLinear = "linear"

// ServiceGrowthTypeExponential indicates exponential growth type in ScalingParams.
const ServiceGrowthTypeExponential = "exponential"

// ServiceServer contains information for one server in the service.
type ServiceServer struct {
	Address string
	Port    int
}

type serviceNode struct {
	address  string
	port     int64
	name     string
	disabled bool
	modified bool
}

// Service represents the mapping from a discovery service into a configuration backend.
type Service struct {
	client        Configuration
	runtime       runtime.Runtime
	name          string
	nodes         []*serviceNode
	usedNames     map[string]struct{}
	transactionID string
	scaling       ScalingParams
	serverParams  models.ServerParams
}

type ServiceI interface {
	NewService(rc runtime.Runtime, name string, scaling ScalingParams, params models.ServerParams) (*Service, error)
	DeleteService(name string)
}

// ScalingParams defines parameter for dynamic server scaling of the Service backend.
type ScalingParams struct {
	BaseSlots       int
	SlotsGrowthType string
	SlotsIncrement  int
}

// NewService creates and returns a new Service instance.
// name indicates the name of the service and only one Service instance with the given name can be created.
func (c *client) NewService(rc runtime.Runtime, name string, scaling ScalingParams, params models.ServerParams) (*Service, error) {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if _, ok := c.services[name]; ok {
		return nil, fmt.Errorf("service with name %s already exists", name)
	}
	service := &Service{
		client:    c,
		runtime:   rc,
		name:      name,
		nodes:     make([]*serviceNode, 0),
		usedNames: make(map[string]struct{}),
		scaling:   scaling,
		serverParams: params,
	}
	c.services[name] = service
	return service, nil
}

// DeleteService removes the Service instance specified by name from the client.
func (c *client) DeleteService(name string) {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	delete(c.services, name)
}

// Delete removes the service from the client with all the associated configuration resources.
func (s *Service) Delete() error {
	err := s.client.DeleteBackend(s.name, s.transactionID, 0)
	if err != nil {
		return err
	}
	s.client.DeleteService(s.name)
	return nil
}

// Init initiates the client by reading the configuration associated with it or created the initial configuration if it does not exist.
func (s *Service) Init(transactionID string, from string) (bool, error) {
	s.SetTransactionID(transactionID)
	newBackend, err := s.createBackend(from)
	if err != nil {
		return false, err
	}
	if newBackend {
		return true, s.createNewNodes(s.scaling.BaseSlots)
	}
	return s.loadNodes()
}

// SetTransactionID updates the transaction ID to be used for modifications on the configuration associated with the service.
func (s *Service) SetTransactionID(transactionID string) {
	s.transactionID = transactionID
}

// UpdateScalingParams updates parameters used for dynamic server scaling of the Service backend
func (s *Service) UpdateScalingParams(scaling ScalingParams) error {
	s.scaling = scaling
	if s.serverCount() < s.scaling.BaseSlots {
		return s.createNewNodes(s.scaling.BaseSlots - s.serverCount())
	}
	return nil
}

// Update updates the backend associated with the server based on the list of servers provided
func (s *Service) Update(servers []ServiceServer) (bool, error) {
	reload := false
	r, err := s.expandNodes(len(servers))
	if err != nil {
		return false, err
	}
	reload = reload || r
	s.markRemovedNodes(servers)
	for _, server := range servers {
		if err = s.handleNode(server); err != nil {
			return false, err
		}
	}
	s.reorderNodes(len(servers))
	r, err = s.updateConfig()
	if err != nil {
		return false, err
	}
	reload = reload || r
	r, err = s.removeExcessNodes(len(servers))

	if err != nil {
		return false, err
	}
	reload = reload || r
	return reload, nil
}

// GetServers returns the list of servers as they are currently configured in the services backend
func (s *Service) GetServers() (models.Servers, error) {
	_, servers, err := s.client.GetServers("backend", s.name, s.transactionID)
	return servers, err
}

func (s *Service) expandNodes(nodeCount int) (bool, error) {
	currentNodeCount := s.serverCount()
	if nodeCount < currentNodeCount {
		return false, nil
	}
	newNodeCount := s.calculateNodeCount(nodeCount)
	if err := s.createNewNodes(newNodeCount - currentNodeCount); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) serverCount() int {
	return len(s.nodes)
}

func (s *Service) calculateNodeCount(nodeCount int) int {
	if s.scaling.SlotsGrowthType == ServiceGrowthTypeLinear {
		return s.calculateNextLinearCount(nodeCount)
	}
	currentNodeCount := s.serverCount()
	for currentNodeCount < nodeCount {
		currentNodeCount *= 2
	}
	return currentNodeCount
}

func (s *Service) calculateNextLinearCount(nodeCount int) int {
	return nodeCount + s.scaling.SlotsIncrement - nodeCount%s.scaling.SlotsIncrement
}

func (s *Service) markRemovedNodes(servers []ServiceServer) {
	for _, node := range s.nodes {
		if node.disabled {
			continue
		}
		if s.nodeRemoved(node, servers) {
			node.modified = true
			node.disabled = true
			node.address = "127.0.0.1"
			node.port = 80
		}
	}
}

func (s *Service) handleNode(server ServiceServer) error {
	if s.serverExists(server) {
		return nil
	}
	return s.setServer(server)
}

func (s *Service) createNewNodes(nodeCount int) error {
	for i := 0; i < nodeCount; i++ {
		if err := s.addNode(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) removeExcessNodes(newNodes int) (bool, error) {
	if newNodes < s.serverCount() {
		if s.serverCount() == s.scaling.BaseSlots {
			return false, nil
		}
		if newNodes < s.scaling.BaseSlots {
			return true, s.removeNodesAfterIndex(s.scaling.BaseSlots)
		}
	}
	lastIndex, reduce := s.getLastNodeIndex(newNodes)
	if !reduce {
		return false, nil
	}
	return true, s.removeNodesAfterIndex(lastIndex)
}

func (s *Service) getLastNodeIndex(nodeCount int) (int, bool) {
	if s.scaling.SlotsGrowthType == ServiceGrowthTypeLinear {
		if nodeCount+s.scaling.SlotsIncrement > s.serverCount() {
			return 0, false
		}
		return s.calculateNextLinearCount(nodeCount), true
	}
	if nodeCount*2 > s.serverCount() {
		return 0, false
	}

	currentNodeCount := s.serverCount()
	for {
		if currentNodeCount/2 < s.scaling.BaseSlots {
			break
		}
		if currentNodeCount/2 <= nodeCount {
			break
		}
		currentNodeCount /= 2
	}
	return currentNodeCount, true
}

func (s *Service) removeNodesAfterIndex(lastIndex int) error {
	for i := lastIndex; i < len(s.nodes); i++ {
		err := s.client.DeleteServer(s.nodes[i].name, "backend", s.name, s.transactionID, 0)
		if err != nil {
			return err
		}

		// Using runtime api to delete server
		err = s.runtime.DisableServer(s.name, s.nodes[i].name)
		if err != nil {
			return err
		}
		err = s.runtime.DeleteServer(s.name, s.nodes[i].name)
		if err != nil {
			return err
		}
	}
	s.nodes = s.nodes[:lastIndex]
	return nil
}

func (s *Service) createBackend(from string) (bool, error) {
	_, _, err := s.client.GetBackend(s.name, s.transactionID)
	if err != nil {
		err := s.client.CreateBackend(&models.Backend{
			From: from,
			Name: s.name,
		}, s.transactionID, 0)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (s *Service) loadNodes() (bool, error) {
	_, servers, err := s.client.GetServers("backend", s.name, s.transactionID)
	if err != nil {
		return false, err
	}
	for _, server := range servers {
		sNode := &serviceNode{
			name:     server.Name,
			address:  server.Address,
			port:     *server.Port,
			modified: false,
		}
		if server.Maintenance == "enabled" {
			sNode.disabled = true
		}
		s.nodes = append(s.nodes, sNode)
	}
	if s.serverCount() < s.scaling.BaseSlots {
		return true, s.createNewNodes(s.scaling.BaseSlots - s.serverCount())
	}
	return false, nil
}

func (s *Service) updateConfig() (bool, error) {
	reload := false
	for _, node := range s.nodes {
		if node.modified {
			server := &models.Server{
				Name:    node.name,
				Address: node.address,
				Port:    misc.Ptr(node.port),
				ServerParams: s.serverParams,
			}
			if node.disabled {
				server.Maintenance = "enabled"
			}
			err := s.client.EditServer(node.name, "backend", s.name, server, s.transactionID, 0)
			if err != nil {
				return false, err
			}

			// Using runtime api to update existing server
			var ras models.RuntimeAddServer
			err = ConvertStruct(server, &ras)
			if err != nil {
				return false, err
			}
			err = s.runtime.DisableServer(s.name, server.Name)
			if err != nil {
				return false, err
			}
			err = s.runtime.DeleteServer(s.name, server.Name)
			if err != nil {
				return false, err
			}
			err = s.runtime.AddServer(s.name, server.Name, SerializeRuntimeAddServer(&ras))
			if err != nil {
				return false, err
			}
			err = s.runtime.EnableHealth(s.name, server.Name)
			if err != nil {
				return false, err
			}
			err = s.runtime.EnableServer(s.name, server.Name)
			if err != nil {
				return false, err
			}

			node.modified = false
			reload = true
		}
	}
	return reload, nil
}

func (s *Service) nodeRemoved(node *serviceNode, servers []ServiceServer) bool {
	for _, server := range servers {
		if s.nodesMatch(node, server) {
			return false
		}
	}
	return true
}

func (s *Service) nodesMatch(sNode *serviceNode, servers ServiceServer) bool {
	return !sNode.disabled && sNode.address == servers.Address && sNode.port == int64(servers.Port)
}

func (s *Service) serverExists(server ServiceServer) bool {
	for _, sNode := range s.nodes {
		if s.nodesMatch(sNode, server) {
			return true
		}
	}
	return false
}

func (s *Service) setServer(server ServiceServer) error {
	for _, sNode := range s.nodes {
		if sNode.disabled {
			sNode.modified = true
			sNode.disabled = false
			sNode.address = server.Address
			sNode.port = int64(server.Port)
			break
		}
	}
	return nil
}

func (s *Service) addNode() error {
	name := s.getNodeName()
	server := &models.Server{
		Name:    name,
		Address: "127.0.0.1",
		Port:    misc.Int64P(80),
		ServerParams: models.ServerParams{
			Weight:      misc.Int64P(128),
			Maintenance: "enabled",
		},
	}
	err := s.client.CreateServer("backend", s.name, server, s.transactionID, 0)
	if err != nil {
		return err
	}

	// Using runtime api to add new servers
	var ras models.RuntimeAddServer
	err = ConvertStruct(server, &ras)
	if err != nil {
		return err
	}
	err = s.runtime.AddServer(s.name, server.Name, SerializeRuntimeAddServer(&ras))
	if err != nil {
		return err
	}

	s.nodes = append(s.nodes, &serviceNode{
		name:     name,
		address:  "127.0.0.1",
		port:     80,
		modified: false,
		disabled: true,
	})
	return nil
}

func (s *Service) getNodeName() string {
	name := fmt.Sprintf("SRV_%s", misc.RandomString(5))
	for _, ok := s.usedNames[name]; ok; {
		name = fmt.Sprintf("SRV_%s", misc.RandomString(5))
	}
	s.usedNames[name] = struct{}{}
	return name
}

func (s *Service) reorderNodes(count int) {
	for i := 0; i < count; i++ {
		if s.nodes[i].disabled {
			s.swapDisabledNode(i)
		}
	}
}

func (s *Service) swapDisabledNode(index int) {
	for i := len(s.nodes) - 1; i > index; i-- {
		if !s.nodes[i].disabled {
			s.nodes[i].disabled = true
			s.nodes[i].modified = true
			s.nodes[index].address = s.nodes[i].address
			s.nodes[index].port = s.nodes[i].port
			s.nodes[index].disabled = false
			s.nodes[index].modified = true
			s.nodes[i].address = "127.0.0.1"
			s.nodes[i].port = 80
			break
		}
	}
}

// ConvertStruct tries to convert a struct from one type to another.
func ConvertStruct[T1 any, T2 any](from T1, to T2) error {
	json := jsoniter.ConfigCompatibleWithStandardLibrary
	js, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(js, to)
}

// SerializeRuntimeAddServer returns a string in the HAProxy config format, suitable
// for the "add server" operation over the control socket.
// Not all the Server attributes are available in this case.
func SerializeRuntimeAddServer(srv *models.RuntimeAddServer) string { //nolint:cyclop,maintidx
	b := &strings.Builder{}

	push := func(s string) {
		b.WriteByte(' ')
		b.WriteString(s)
	}
	pushi := func(key string, val *int64) {
		fmt.Fprintf(b, " %s %d", key, *val)
	}
	// push a quoted string
	pushq := func(key, val string) {
		fmt.Fprintf(b, ` %s %s`, key, val)
	}
	enabled := func(s string) bool {
		return s == "enabled"
	}

	// Address is mandatory and must come first, with an optional port number.
	addr := srv.Address
	if srv.Port != nil {
		addr += fmt.Sprintf(":%d", *srv.Port)
	}
	push(addr)

	if enabled(srv.AgentCheck) {
		push("agent-check")
	}
	if srv.AgentAddr != "" {
		pushq("agent-addr", srv.AgentAddr)
	}
	if srv.AgentPort != nil {
		pushi("agent-port", srv.AgentPort)
	}
	if srv.AgentInter != nil {
		pushi("agent-inter", srv.AgentInter)
	}
	if srv.AgentSend != "" {
		pushq("agent-send", srv.AgentSend)
	}
	if srv.Allow0rtt {
		push("allow-0rtt")
	}
	if srv.Alpn != "" {
		pushq("alpn", srv.Alpn)
	}
	if enabled(srv.Backup) {
		push("backup")
	}
	if srv.SslCafile != "" {
		pushq("ca-file", srv.SslCafile)
	}
	if enabled(srv.Check) {
		push("check")
	}
	if srv.CheckAlpn != "" {
		pushq("check-alpn", srv.CheckAlpn)
	}
	if srv.HealthCheckAddress != "" {
		pushq("addr", srv.HealthCheckAddress)
	}
	if srv.HealthCheckPort != nil {
		pushi("port", srv.HealthCheckPort)
	}
	if srv.CheckProto != "" {
		pushq("check-proto", srv.CheckProto)
	}
	if enabled(srv.CheckSendProxy) {
		push("check-send-proxy")
	}
	if srv.CheckSni != "" {
		pushq("check-sni", srv.CheckSni)
	}
	if enabled(srv.CheckSsl) {
		push("check-ssl")
	}
	if enabled(srv.CheckViaSocks4) {
		push("check-via-socks4")
	}
	if srv.Ciphers != "" {
		pushq("ciphers", srv.Ciphers)
	}
	if srv.Ciphersuites != "" {
		pushq("ciphersuites", srv.Ciphersuites)
	}
	if srv.CrlFile != "" {
		pushq("crl-file", srv.CrlFile)
	}
	if enabled(srv.Maintenance) {
		push("disabled")
	}
	if srv.Downinter != nil {
		pushi("downinter", srv.Downinter)
	}
	if !enabled(srv.Maintenance) {
		push("enabled")
	}
	if srv.ErrorLimit != nil {
		pushi("error-limit", srv.ErrorLimit)
	}
	if srv.Fall != nil {
		pushi("fall", srv.Fall)
	}
	if srv.Fastinter != nil {
		pushi("fastinter", srv.Fastinter)
	}
	if enabled(srv.ForceSslv3) {
		push("force-sslv3")
	}
	if enabled(srv.ForceTlsv10) {
		push("force-tlsv10")
	}
	if enabled(srv.ForceTlsv11) {
		push("force-tlsv11")
	}
	if enabled(srv.ForceTlsv12) {
		push("force-tlsv12")
	}
	if enabled(srv.ForceTlsv13) {
		push("force-tlsv13")
	}
	if srv.ID != "" {
		pushq("id", srv.ID)
	}
	if srv.Inter != nil {
		pushi("inter", srv.Inter)
	}
	if srv.Maxconn != nil {
		pushi("maxconn", srv.Maxconn)
	}
	if srv.Maxqueue != nil {
		pushi("maxqueue", srv.Maxqueue)
	}
	if srv.Minconn != nil {
		pushi("minconn", srv.Minconn)
	}
	if !enabled(srv.SslReuse) {
		push("no-ssl-reuse")
	}
	if enabled(srv.NoSslv3) {
		push("no-sslv3")
	}
	if enabled(srv.NoTlsv10) {
		push("no-tlsv10")
	}
	if enabled(srv.NoTlsv11) {
		push("no-tlsv11")
	}
	if enabled(srv.NoTlsv12) {
		push("no-tlsv12")
	}
	if enabled(srv.NoTlsv13) {
		push("no-tlsv13")
	}
	if !enabled(srv.TLSTickets) {
		push("no-tls-tickets")
	}
	if srv.Npn != "" {
		pushq("npm", srv.Npn)
	}
	if srv.Observe != "" {
		pushq("observe", srv.Observe)
	}
	if srv.OnError != "" {
		pushq("on-error", srv.OnError)
	}
	if srv.OnMarkedDown != "" {
		pushq("on-marked-down", srv.OnMarkedDown)
	}
	if srv.OnMarkedUp != "" {
		pushq("on-marked-up", srv.OnMarkedUp)
	}
	if srv.PoolLowConn != nil {
		pushi("pool-low-conn", srv.PoolLowConn)
	}
	if srv.PoolMaxConn != nil {
		pushi("pool-max-conn", srv.PoolMaxConn)
	}
	if srv.PoolPurgeDelay != nil {
		pushi("pool-purge-delay", srv.PoolPurgeDelay)
	}
	if srv.Proto != "" {
		pushq("proto", srv.Proto)
	}
	if len(srv.ProxyV2Options) > 0 {
		pushq("proxy-v2-options", strings.Join(srv.ProxyV2Options, ","))
	}
	if srv.Rise != nil {
		pushi("rise", srv.Rise)
	}
	if enabled(srv.SendProxy) {
		push("send-proxy")
	}
	if enabled(srv.SendProxyV2) {
		push("send-proxy-v2")
	}
	if enabled(srv.SendProxyV2Ssl) {
		push("send-proxy-v2-ssl")
	}
	if enabled(srv.SendProxyV2SslCn) {
		push("send-proxy-v2-ssl-cn")
	}
	if srv.Slowstart != nil {
		pushi("slowstart", srv.Slowstart)
	}
	if srv.Sni != "" {
		pushq("sni", srv.Sni)
	}
	if srv.Source != "" {
		pushq("source", srv.Source)
	}
	if enabled(srv.Ssl) {
		push("ssl")
	}
	if srv.SslMaxVer != "" {
		pushq("ssl-max-ver", srv.SslMaxVer)
	}
	if srv.SslMinVer != "" {
		pushq("ssl-min-ver", srv.SslMinVer)
	}
	if enabled(srv.Tfo) {
		push("tfo")
	}
	if enabled(srv.TLSTickets) {
		push("tls-tickets")
	}
	if srv.Track != "" {
		pushq("track", srv.Track)
	}
	if srv.Verify != "" {
		pushq("verify", srv.Verify)
	}
	if srv.Verifyhost != "" {
		pushq("verifyhost", srv.Verifyhost)
	}
	if srv.Weight != nil {
		pushi("weight", srv.Weight)
	}
	if srv.Ws != "" {
		pushq("ws", srv.Ws)
	}

	return b.String()
}
