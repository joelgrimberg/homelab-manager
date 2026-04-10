package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProxmoxClient talks to the Proxmox REST API using token auth.
type ProxmoxClient struct {
	baseURL string
	node    string
	token   string // "PVEAPIToken=USER!TOKENID=SECRET"
	http    *http.Client
}

func NewProxmoxClient(cfg ProxmoxConfig) *ProxmoxClient {
	return &ProxmoxClient{
		baseURL: cfg.URL,
		node:    cfg.Node,
		token:   fmt.Sprintf("PVEAPIToken=%s=%s", cfg.TokenID, cfg.TokenSecret),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

type proxmoxResponse struct {
	Data json.RawMessage `json:"data"`
}

type vmStatus struct {
	Status string `json:"status"`
}

func (c *ProxmoxClient) apiPath(inst Instance) string {
	kind := "qemu"
	if inst.Type == "lxc" {
		kind = "lxc"
	}
	return fmt.Sprintf("/api2/json/nodes/%s/%s/%d", c.node, kind, inst.VMID)
}

func (c *ProxmoxClient) do(method, path string) ([]byte, error) {
	url := c.baseURL + path
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API %s %s returned %d: %s", method, path, resp.StatusCode, string(body))
	}

	return body, nil
}

// GetStatus returns the status string ("running", "stopped", etc.) for an instance.
func (c *ProxmoxClient) GetStatus(inst Instance) (string, error) {
	body, err := c.do("GET", c.apiPath(inst)+"/status/current")
	if err != nil {
		return "", err
	}

	var resp proxmoxResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parsing status response: %w", err)
	}

	var status vmStatus
	if err := json.Unmarshal(resp.Data, &status); err != nil {
		return "", fmt.Errorf("parsing status data: %w", err)
	}

	return status.Status, nil
}

// Start sends a start command to an instance.
func (c *ProxmoxClient) Start(inst Instance) error {
	_, err := c.do("POST", c.apiPath(inst)+"/status/start")
	return err
}

// Stop sends a graceful shutdown command to an instance.
func (c *ProxmoxClient) Stop(inst Instance) error {
	_, err := c.do("POST", c.apiPath(inst)+"/status/shutdown")
	return err
}

// ProxmoxInstance represents a VM or LXC as returned by the Proxmox list API.
type ProxmoxInstance struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Tags   string `json:"tags"`
	Type   string // set by caller: "qemu" or "lxc"
}

// ListVMs returns all QEMU VMs on the configured node.
func (c *ProxmoxClient) ListVMs() ([]ProxmoxInstance, error) {
	return c.listInstances("qemu")
}

// ListLXCs returns all LXC containers on the configured node.
func (c *ProxmoxClient) ListLXCs() ([]ProxmoxInstance, error) {
	return c.listInstances("lxc")
}

func (c *ProxmoxClient) listInstances(kind string) ([]ProxmoxInstance, error) {
	path := fmt.Sprintf("/api2/json/nodes/%s/%s", c.node, kind)
	body, err := c.do("GET", path)
	if err != nil {
		return nil, err
	}

	var resp proxmoxResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing %s list response: %w", kind, err)
	}

	var instances []ProxmoxInstance
	if err := json.Unmarshal(resp.Data, &instances); err != nil {
		return nil, fmt.Errorf("parsing %s list data: %w", kind, err)
	}

	for i := range instances {
		instances[i].Type = kind
	}

	return instances, nil
}
