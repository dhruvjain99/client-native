package configuration

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/haproxytech/config-parser/params"

	strfmt "github.com/go-openapi/strfmt"
	parser "github.com/haproxytech/config-parser"
	parser_errors "github.com/haproxytech/config-parser/errors"
	"github.com/haproxytech/config-parser/types"
	"github.com/haproxytech/models"
)

// GetServers returns a struct with configuration version and an array of
// configured servers in the specified backend. Returns error on fail.
func (c *Client) GetServers(backend string, transactionID string) (*models.GetServersOKBody, error) {
	p, err := c.GetParser(transactionID)
	if err != nil {
		return nil, err
	}

	servers, err := c.parseServers(backend, p)
	if err != nil {
		return nil, c.handleError("", "backend", backend, "", false, err)
	}

	v, err := c.GetVersion(transactionID)
	if err != nil {
		return nil, err
	}

	return &models.GetServersOKBody{Version: v, Data: servers}, nil
}

// GetServer returns a struct with configuration version and a requested server
// in the specified backend. Returns error on fail or if server does not exist.
func (c *Client) GetServer(name string, backend string, transactionID string) (*models.GetServerOKBody, error) {
	p, err := c.GetParser(transactionID)
	if err != nil {
		return nil, err
	}

	server, _ := c.getServerByName(name, backend, p)
	if server == nil {
		return nil, NewConfError(ErrObjectDoesNotExist, fmt.Sprintf("Server %s does not exist in backend %s", name, backend))
	}

	v, err := c.GetVersion(transactionID)
	if err != nil {
		return nil, err
	}

	return &models.GetServerOKBody{Version: v, Data: server}, nil
}

// DeleteServer deletes a server in configuration. One of version or transactionID is
// mandatory. Returns error on fail, nil on success.
func (c *Client) DeleteServer(name string, backend string, transactionID string, version int64) error {
	p, t, err := c.loadDataForChange(transactionID, version)
	if err != nil {
		return err
	}

	server, i := c.getServerByName(name, backend, p)
	if server == nil {
		e := NewConfError(ErrObjectDoesNotExist, fmt.Sprintf("Server %s does not exist in backend %s", name, backend))
		return c.handleError(name, "backend", backend, t, transactionID == "", e)
	}

	if err := p.Delete(parser.Backends, backend, "server", i); err != nil {
		return c.handleError(name, "backend", backend, t, transactionID == "", err)
	}

	if err := c.saveData(p, t, transactionID == ""); err != nil {
		return err
	}

	return nil
}

// CreateServer creates a server in configuration. One of version or transactionID is
// mandatory. Returns error on fail, nil on success.
func (c *Client) CreateServer(backend string, data *models.Server, transactionID string, version int64) error {
	if c.UseValidation {
		validationErr := data.Validate(strfmt.Default)
		if validationErr != nil {
			return NewConfError(ErrValidationError, validationErr.Error())
		}
	}
	p, t, err := c.loadDataForChange(transactionID, version)
	if err != nil {
		return err
	}

	server, _ := c.getServerByName(data.Name, backend, p)
	if server != nil {
		e := NewConfError(ErrObjectAlreadyExists, fmt.Sprintf("Server %s already exists in backend %s", data.Name, backend))
		return c.handleError(data.Name, "backend", backend, t, transactionID == "", e)
	}

	if err := p.Insert(parser.Backends, backend, "server", serializeServer(*data), -1); err != nil {
		return c.handleError(data.Name, "backend", backend, t, transactionID == "", err)
	}

	if err := c.saveData(p, t, transactionID == ""); err != nil {
		return err
	}
	return nil
}

// EditServer edits a server in configuration. One of version or transactionID is
// mandatory. Returns error on fail, nil on success.
func (c *Client) EditServer(name string, backend string, data *models.Server, transactionID string, version int64) error {
	if c.UseValidation {
		validationErr := data.Validate(strfmt.Default)
		if validationErr != nil {
			return NewConfError(ErrValidationError, validationErr.Error())
		}
	}
	p, t, err := c.loadDataForChange(transactionID, version)
	if err != nil {
		return err
	}

	server, i := c.getServerByName(name, backend, p)
	if server == nil {
		e := NewConfError(ErrObjectDoesNotExist, fmt.Sprintf("Server %v does not exist in backend %s", name, backend))
		return c.handleError(data.Name, "backend", backend, t, transactionID == "", e)
	}

	if err := p.Set(parser.Backends, backend, "server", serializeServer(*data), i); err != nil {
		return c.handleError(data.Name, "backend", backend, t, transactionID == "", err)
	}

	if err := c.saveData(p, t, transactionID == ""); err != nil {
		return err
	}
	return nil
}

func (c *Client) parseServers(backend string, p *parser.Parser) (models.Servers, error) {
	servers := models.Servers{}

	data, err := p.Get(parser.Backends, backend, "server", false)
	if err != nil {
		if err == parser_errors.FetchError {
			return servers, nil
		}
		return nil, err
	}

	ondiskServers := data.([]types.Server)
	for _, ondiskServer := range ondiskServers {
		s := parseServer(ondiskServer)
		if s != nil {
			servers = append(servers, s)
		}
	}
	return servers, nil
}

func parseServer(ondiskServer types.Server) *models.Server {
	s := &models.Server{
		Name: ondiskServer.Name,
	}
	addSlice := strings.Split(ondiskServer.Address, ":")
	if len(addSlice) == 0 {
		return nil
	} else if len(addSlice) > 1 {
		s.Address = addSlice[0]
		if addSlice[1] != "" {
			p, err := strconv.ParseInt(addSlice[1], 10, 64)
			if err == nil {
				s.Port = &p
			}
		}
	} else if len(addSlice) > 0 {
		s.Address = addSlice[0]
	}
	for _, p := range ondiskServer.Params {
		switch v := p.(type) {
		case *params.ServerOptionWord:
			switch v.Name {
			case "backup":
				s.Backup = "enabled"
			case "no-backup":
				s.Backup = "disabled"
			case "disabled":
				s.Maintenance = "enabled"
			case "enabled":
				s.Maintenance = "disabled"
			case "check":
				s.Check = "enabled"
			case "no-check":
				s.Check = "disabled"
			case "ssl":
				s.Ssl = "enabled"
			case "no-ssl":
				s.Ssl = "disabled"
			case "tls-tickets":
				s.TLSTickets = "enabled"
			case "no-tls-tickets":
				s.TLSTickets = "disabled"
			}
		case *params.ServerOptionValue:
			switch v.Name {
			case "maxconn":
				m, err := strconv.ParseInt(v.Value, 10, 64)
				if err == nil && m != 0 {
					s.Maxconn = &m
				}
			case "weight":
				w, err := strconv.ParseInt(v.Value, 10, 64)
				if err == nil && w != 0 {
					s.Weight = &w
				}
			case "cookie":
				s.Cookie = v.Value
			case "crt":
				s.SslCertificate = v.Value
			case "ca-file":
				s.SslCafile = v.Value
			}
		}
	}
	return s
}

func serializeServer(s models.Server) types.Server {
	srv := types.Server{
		Name:   s.Name,
		Params: []params.ServerOption{},
	}
	if s.Port != nil {
		srv.Address = s.Address + ":" + strconv.FormatInt(*s.Port, 10)
	} else {
		srv.Address = s.Address
	}
	if s.Backup == "enabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "backup"})
	}
	if s.Backup == "disabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "no-backup"})
	}
	if s.Maintenance == "enabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "disabled"})
	}
	if s.Maintenance == "disabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "enabled"})
	}
	if s.Check == "enabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "check"})
	}
	if s.Check == "disabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "no-check"})
	}
	if s.Ssl == "enabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "ssl"})
	}
	if s.Ssl == "disabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "no-ssl"})
	}
	if s.TLSTickets == "enabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "tls-tickets"})
	}
	if s.TLSTickets == "disabled" {
		srv.Params = append(srv.Params, &params.ServerOptionWord{Name: "no-tls-tickets"})
	}
	if s.Maxconn != nil {
		srv.Params = append(srv.Params, &params.ServerOptionValue{Name: "maxconn", Value: strconv.FormatInt(*s.Maxconn, 10)})
	}
	if s.Weight != nil {
		srv.Params = append(srv.Params, &params.ServerOptionValue{Name: "weight", Value: strconv.FormatInt(*s.Weight, 10)})
	}
	if s.Cookie != "" {
		srv.Params = append(srv.Params, &params.ServerOptionValue{Name: "cookie", Value: s.Cookie})
	}
	if s.SslCertificate != "" {
		srv.Params = append(srv.Params, &params.ServerOptionValue{Name: "crt", Value: s.SslCertificate})
	}
	if s.SslCafile != "" {
		srv.Params = append(srv.Params, &params.ServerOptionValue{Name: "ca-file", Value: s.SslCafile})
	}
	return srv
}

func (c *Client) getServerByName(name string, backend string, p *parser.Parser) (*models.Server, int) {
	servers, err := c.parseServers(backend, p)
	if err != nil {
		return nil, 0
	}

	for i, s := range servers {
		if s.Name == name {
			return s, i
		}
	}
	return nil, 0
}
