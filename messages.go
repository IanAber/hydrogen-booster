package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Message is an operator-facing fault or error message in a form the
// web UI can translate: Code selects a template in web/i18n.js (key
// "msg_<code>"), and Params fill its {placeholders}. A param value is
// either a plain string (numbers are pre-formatted here), a sensor or
// digital input name under the "sensor" param (translated via
// "sensor_<name>"), or a nested Message, translated recursively. Text
// is the English rendering - used in logs, plain-text API responses,
// and as the UI's fallback for a code (or language) it doesn't know.
type Message struct {
	Code   string         `json:"code,omitempty"`
	Params map[string]any `json:"params,omitempty"`
	Text   string         `json:"text"`
}

// msg builds a Message whose English Text is format/args.
func msg(code string, params map[string]any, format string, args ...any) Message {
	return Message{Code: code, Params: params, Text: fmt.Sprintf(format, args...)}
}

// msgError is an error carrying a Message, so the code and params
// survive the trip up to an HTTP handler (see writeError) - plain
// errors still work everywhere, they just aren't translated.
type msgError struct {
	Message
	wrapped error
}

func (e *msgError) Error() string { return e.Text }
func (e *msgError) Unwrap() error { return e.wrapped }

func msgErr(code string, params map[string]any, format string, args ...any) error {
	return &msgError{Message: msg(code, params, format, args...)}
}

// messageOf recovers err's Message, or wraps a plain error's text as an
// untranslated one (e.g. an OS or driver error string).
func messageOf(err error) Message {
	var me *msgError
	if errors.As(err, &me) {
		return me.Message
	}
	return Message{Text: err.Error()}
}

// writeError replies with err's English text as the plain-text body,
// as before, plus its Message (code/params) in the X-Message header,
// URL-escaped JSON, so a page can show it in the operator's language
// without changing the response for any other API client. The
// rejection is logged too, so the journal shows e.g. why a start was
// refused.
func writeError(rw http.ResponseWriter, r *http.Request, err error, status int) {
	logWarnf("Rejected %s %s: %v", r.Method, r.URL.Path, err)
	if b, jerr := json.Marshal(messageOf(err)); jerr == nil {
		rw.Header().Set("X-Message", url.PathEscape(string(b)))
	}
	http.Error(rw, err.Error(), status)
}
