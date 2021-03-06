/*

	Package raven is priveds a client and library for sending messages and exceptions to Sentry: http://getsentry.com

	Usage:

	Create a new client using the NewClient() function. The value for the DSN parameter can be obtained
	from the project page in the Sentry web interface. After the client has been created use the CaptureMessage
	method to send messages to the server.

		client, err := raven.NewClient(dsn)
		...
		id, err := self.CaptureMessage("some text")


*/
package raven

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	UDP_TEMPLATE = "%s\n\n%s"
)

type SentryTransport interface {
	Send(packet []byte, timestamp time.Time) (response string, err error)
}

type HttpClient interface {
	Do(req *http.Request) (resp *http.Response, err error)
}

type HttpSentryTransport struct {
	PublicKey string
	URL       *url.URL
	Project   string
	Client    HttpClient
}

type UdpSentryTransport struct {
	PublicKey string
	URL       *url.URL
	Client    net.Conn
}

func (self *UdpSentryTransport) Send(packet []byte, timestamp time.Time) (response string, err error) {
	authHeader := AuthHeader(timestamp, self.PublicKey)
	udp_msg := fmt.Sprintf(UDP_TEMPLATE, authHeader, string(packet))
	self.Client.Write([]byte(udp_msg))

	return "", nil
}

func (self *HttpSentryTransport) Send(packet []byte, timestamp time.Time) (response string, err error) {
	apiURL := self.URL
	apiURL.Path = path.Join(apiURL.Path, "/api/"+self.Project+"/store/")
	apiURL.User = nil

	// Append slash to prevent 301 redirect
	location := strings.TrimRight(apiURL.String(), "/") + "/"

	// for loop to follow redirects
	for {
		buf := bytes.NewBuffer(packet)
		req, err := http.NewRequest("POST", location, buf)
		if err != nil {
			return "", err
		}

		authHeader := AuthHeader(timestamp, self.PublicKey)
		req.Header.Add("X-Sentry-Auth", authHeader)
		req.Header.Add("Content-Type", "application/octet-stream")
		req.Header.Add("Connection", "close")
		req.Header.Add("Accept-Encoding", "identity")

		resp, err := self.Client.Do(req)
		if err != nil {
			return "", err
		}

		if resp.StatusCode == 301 {
			// set the location to the new one to retry on the next iteration
			location = resp.Header["Location"][0]
		} else {

			// We want to return an error for anything that's not a
			// straight HTTP 200
			if resp.StatusCode != 200 {
				body, _ := ioutil.ReadAll(resp.Body)
				return string(body), errors.New(resp.Status)
			}
			body, _ := ioutil.ReadAll(resp.Body)
			return string(body), nil
		}
	}
	// should never get here
	panic("send broke out of loop")
}

type Client struct {
	URL       *url.URL
	PublicKey string
	SecretKey string
	Project   string
	Logger    string

	sentryTransport SentryTransport
}

type sentryRequest struct {
	Project   string `json:"project"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Logger    string `json:"logger"`
}

type sentryResponse struct {
	ResultId string `json:"result_id"`
}

// Template for the X-Sentry-Auth header
const xSentryAuthTemplate = "Sentry sentry_version=2.0, sentry_client=raven-go/0.1, sentry_timestamp=%v, sentry_key=%v"

// An iso8601 timestamp without the timezone. This is the format Sentry expects.
const iso8601 = "2006-01-02T15:04:05"

// NewClient creates a new client for a server identified by the given dsn
// A dsn is a string in the form:
//	{PROTOCOL}://{PUBLIC_KEY}:{SECRET_KEY}@{HOST}/{PATH}{PROJECT_ID}
// eg:
//	http://abcd:efgh@sentry.example.com/sentry/project1
func NewClient(dsn string, logger string) (self *Client, err error) {
	var sentryTransport SentryTransport

	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}

	basePath := path.Dir(u.Path)
	project := path.Base(u.Path)
	publicKey := u.User.Username()
	secretKey, _ := u.User.Password()
	u.Path = basePath

	check := func(req *http.Request, via []*http.Request) error {
		fmt.Printf("%+v", req)
		return nil
	}

	switch {
	case u.Scheme == "udp":
		udp_conn, udp_err := net.Dial("udp", u.Host)
		if udp_err != nil {
			return nil, udp_err
		}
		sentryTransport = &UdpSentryTransport{URL: u,
			Client:    udp_conn,
			PublicKey: publicKey}
	case u.Scheme == "http":
		httpClient := &http.Client{nil, check, nil}
		sentryTransport = &HttpSentryTransport{URL: u,
			Client:    httpClient,
			Project:   project,
			PublicKey: publicKey}
	}

	return &Client{URL: u, PublicKey: publicKey, SecretKey: secretKey,
		sentryTransport: sentryTransport, Project: project, Logger: logger}, nil
}

// Sends a message to the sentry server with level "debug"
func (self *Client) Debug(message ...string) (err error) {
	return self.captureMessage("debug", message)
}

// Sends a message to the sentry server with level "info"
func (self *Client) Info(message ...string) (err error) {
	return self.captureMessage("info", message)
}

// Sends a message to the sentry server with level "warning"
func (self *Client) Warning(message ...string) (err error) {
	return self.captureMessage("warning", message)
}

// Sends a message to the sentry server with level "error"
func (self *Client) Error(message ...string) (err error) {
	return self.captureMessage("error", message)
}

// Sends a message to the sentry server with level "fatal"
func (self *Client) Fatal(message ...string) (err error) {
	return self.captureMessage("fatal", message)
}

// CaptureMessage sends a message to the Sentry server.
func (self *Client) captureMessage(level string, message []string) (err error) {
	timestamp := time.Now().UTC()
	timestampStr := timestamp.Format(iso8601)

	packet := sentryRequest{
		Project:   self.Project,
		Message:   strings.Join(message, " "),
		Timestamp: timestampStr,
		Level:     level,
		Logger:    self.Logger,
	}

	buf := new(bytes.Buffer)
	b64Encoder := base64.NewEncoder(base64.StdEncoding, buf)
	writer := zlib.NewWriter(b64Encoder)
	jsonEncoder := json.NewEncoder(writer)

	if err := jsonEncoder.Encode(packet); err != nil {
		return err
	}

	err = writer.Close()
	if err != nil {
		return err
	}

	err = b64Encoder.Close()
	if err != nil {
		return err
	}

	_, ok := self.sentryTransport.Send(buf.Bytes(), timestamp)
	if ok != nil {
		return err
	}

	return nil
}

/* Compute the Sentry authentication header */
func AuthHeader(timestamp time.Time, publicKey string) string {
	return fmt.Sprintf(xSentryAuthTemplate, timestamp.Unix(),
		publicKey)
}
