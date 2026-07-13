package panel

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/go-resty/resty/v2"
	"github.com/limo13660/daonode/conf"
)

// Panel is the interface for different panel's api.

type Client struct {
	client           *resty.Client
	reportClient     *resty.Client
	APIHost          string
	Token            string
	NodeId           int
	nodeEtag         string
	userEtag         string
	responseBodyHash string
	UserList         *UserListBody
	AliveMap         *AliveMap
}

func New(c *conf.NodeConfig) (*Client, error) {
	client := resty.New()
	client.SetLogger(redactingRestyLogger{token: c.Key})
	retryCount := conf.DefaultNodeRetryCount
	if c.RetryCount != nil {
		retryCount = *c.RetryCount
	}
	client.SetRetryCount(retryCount)
	client.SetHeader("User-Agent", fmt.Sprintf("daonode go-resty/%s (https://github.com/go-resty/resty)", resty.Version))
	if c.Timeout > 0 {
		client.SetTimeout(time.Duration(c.Timeout) * time.Second)
	} else {
		client.SetTimeout(time.Duration(conf.DefaultNodeTimeout) * time.Second)
	}
	client.SetBaseURL(c.APIHost)
	// set params
	client.SetQueryParams(map[string]string{
		"node_type": "daonode",
		"node_id":   strconv.Itoa(c.NodeID),
		"token":     c.Key,
	})
	// Do not automatically replay traffic and online-user POST requests inside
	// one Resty call. A panel may have accepted a request whose response was
	// lost, so an immediate transport retry increases duplicate-accounting risk.
	reportClient := client.Clone().SetRetryCount(0)
	return &Client{
		client:       client,
		reportClient: reportClient,
		Token:        c.Key,
		APIHost:      c.APIHost,
		NodeId:       c.NodeID,
		UserList:     &UserListBody{},
		AliveMap:     &AliveMap{},
	}, nil
}

type redactingRestyLogger struct {
	token string
}

func (l redactingRestyLogger) Errorf(format string, values ...interface{}) {
	logrus.Error(l.redact(fmt.Sprintf(format, values...)))
}

func (l redactingRestyLogger) Warnf(format string, values ...interface{}) {
	logrus.Warn(l.redact(fmt.Sprintf(format, values...)))
}

func (l redactingRestyLogger) Debugf(format string, values ...interface{}) {
	logrus.Debug(l.redact(fmt.Sprintf(format, values...)))
}

func (l redactingRestyLogger) redact(message string) string {
	return redactSecret(message, l.token)
}

type panelRequestError struct {
	err   error
	token string
}

func (e panelRequestError) Error() string {
	return redactSecret(e.err.Error(), e.token)
}

func (e panelRequestError) Unwrap() error {
	return e.err
}

func (c *Client) requestError(err error) error {
	if err == nil {
		return nil
	}
	return panelRequestError{err: err, token: c.Token}
}

func redactSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	message = strings.ReplaceAll(message, secret, "[redacted]")
	if escaped := url.QueryEscape(secret); escaped != secret {
		message = strings.ReplaceAll(message, escaped, "[redacted]")
	}
	return message
}
