package serve

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

func onionCertificateURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != schemeHTTPS && u.Scheme != schemeWSS) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return len(host) > len(".onion") && strings.HasSuffix(host, ".onion")
}

func startOnionCertificates(ctx context.Context, port int) error {
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := dialBrowser(setup, port)
	if err != nil {
		return err
	}
	if err := enableOnionCertificates(setup, c); err != nil {
		c.close()
		return err
	}
	go func() {
		defer c.close()
		// Like the dialog watcher, this must survive shutdown's state capture.
		if err := runOnionCertificates(context.WithoutCancel(ctx), c); err != nil {
			logWarn("onion certificate handler disconnected (port=%d); certificate checks revert to browser defaults", port)
		}
	}()
	return nil
}

func enableOnionCertificates(ctx context.Context, c *cdpConn) error {
	for _, cmd := range []struct {
		method string
		params map[string]any
	}{
		{"Security.enable", nil},
		{"Security.setOverrideCertificateErrors", map[string]any{"override": true}},
		// Forces page agent creation synchronously in Chrome, including future tabs.
		// The browser-level handler is already installed before any page can navigate.
		{"Target.setAutoAttach", autoAttachPages()},
	} {
		id, err := c.send(ctx, "", cmd.method, cmd.params)
		if err != nil {
			return err
		}
		var resultErr error
		err = c.await(ctx, func(msg map[string]any, _ []byte) bool {
			if resultErr = handleOnionCertificate(ctx, c, msg); resultErr != nil {
				return true
			}
			got, _ := asInt(msg[cdpID])
			if got != id {
				return false
			}
			if msg["error"] != nil {
				resultErr = fmt.Errorf("%w: %s rejected", errCDPCall, cmd.method)
			}
			return true
		})
		if err != nil {
			return err
		}
		if resultErr != nil {
			return resultErr
		}
	}
	return nil
}

func runOnionCertificates(ctx context.Context, c *cdpConn) error {
	var handlerErr error
	err := c.await(ctx, func(msg map[string]any, _ []byte) bool {
		handlerErr = handleOnionCertificate(ctx, c, msg)
		return handlerErr != nil
	})
	if err != nil {
		return err
	}
	return handlerErr
}

func handleOnionCertificate(ctx context.Context, c *cdpConn, msg map[string]any) error {
	// Chromium can hold a popup even when attachedToTarget reports waitingForDebugger=false.
	if msg[cdpMethod] == "Target.attachedToTarget" {
		params, _ := msg[cdpParams].(map[string]any)
		_, err := c.send(ctx, asString(params[cdpSessionID]), "Runtime.runIfWaitingForDebugger", nil)
		return err
	}
	if msg[cdpMethod] != "Security.certificateError" {
		return nil
	}
	params, _ := msg[cdpParams].(map[string]any)
	eventID, ok := asInt(params["eventId"])
	if !ok {
		return fmt.Errorf("%w: missing certificate event id", errCDPCall)
	}
	action := "cancel"
	if onionCertificateURL(asString(params["requestURL"])) {
		action = "continue"
	}
	_, err := c.send(ctx, "", "Security.handleCertificateError", map[string]any{"eventId": eventID, "action": action})
	return err
}
