package core

import (
	// "errors"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"

	"github.com/toorop/tmail/msproto"
)

type onfailure int

// what to do on failure
const (
	CONTINUE onfailure = 1 + iota
	TEMPFAIL
	PERMFAIL
)

// microservice represents a microservice
type microservice struct {
	url                  string
	skipAuthentifiedUser bool
	fireAndForget        bool
	timeout              uint64
	onFailure            onfailure
}

// newMicroservice retuns a microservice parsing URI
func newMicroservice(uri string) (*microservice, error) {
	ms := &microservice{
		skipAuthentifiedUser: false,
		onFailure:            CONTINUE,
		timeout:              30,
	}
	t := strings.Split(uri, "?")
	ms.url = t[0]
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	if parsed.Query().Get("skipauthentifieduser") == "true" {
		ms.skipAuthentifiedUser = true
	}

	if parsed.Query().Get("fireandforget") == "true" {
		ms.fireAndForget = true
	}
	if parsed.Query().Get("timeout") != "" {
		ms.timeout, err = strconv.ParseUint(parsed.Query().Get("timeout"), 10, 64)
		if err != nil {
			return nil, err
		}
	}

	if parsed.Query().Get("onfailure") != "" {
		switch parsed.Query().Get("onfailure") {
		case "tempfail":
			ms.onFailure = TEMPFAIL
		case "permfail":
			ms.onFailure = PERMFAIL
		}
	}
	return ms, nil
}

// doRequest do request on microservices endpoint
func (ms *microservice) doRequest(data *[]byte) (*http.Response, error) {
	req, _ := http.NewRequest("POST", ms.url, bytes.NewBuffer(*data))
	req.Header.Set("Content-Type", "application/x-protobuf")
	client := &http.Client{
		Timeout: time.Duration(ms.timeout) * time.Second,
	}
	return client.Do(req)
}

// call will call microservice
func (ms *microservice) call(data *[]byte) (*[]byte, error) {
	r, err := ms.doRequest(data)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	// always get returned data
	rawBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	// HTTP error handling
	if r.StatusCode > 399 {
		return nil, errors.New(r.Status + " - " + string(rawBody))
	}
	return &rawBody, nil
}

// shouldWeStopOnError return if process wich call microserviuce should stop on error
func (ms *microservice) stopOnError() (stop bool) {
	switch ms.onFailure {
	case PERMFAIL:
		return true
	case TEMPFAIL:
		return true
	default:
		return false
	}
}

// smtpdStopOnError handle error for smtpd microservice
// it returns true if tmail must stop processing other ms
// handleSMTPError
func (ms *microservice) handleSMTPError(err error, s *SMTPServerSession) (stop bool) {
	if err == nil {
		return false
	}
	s.logError("microservice " + ms.url + " failed. " + err.Error())
	switch ms.onFailure {
	case PERMFAIL:
		s.out("550 sorry something wrong happened")
		return true
	case TEMPFAIL:
		s.out("450 sorry something wrong happened")
		return true
	default:
		return false
	}
}

// handleSmtpResponse common handling of msproto.SmtpdResponse
func handleSMTPResponse(smtpResponse *msproto.SmtpResponse, s *SMTPServerSession) (stop bool) {
	if smtpResponse == nil {
		return
	}
	if smtpResponse.GetCode() != 0 && smtpResponse.GetMsg() != "" {
		reply := fmt.Sprintf("%d %s", smtpResponse.GetCode(), smtpResponse.GetMsg())
		s.out(reply)
		s.log("smtp response from microservice sent to client: " + reply)
		// if reply is sent we do not continue processing this command
		stop = true
	}
	return
}

// msSmtpdNewClient execute microservices for smtpdnewclient hook
// Warning: error are not returned to client
func msSmtpdNewClient(s *SMTPServerSession) (stop bool) {
	if len(Cfg.GetMicroservicesUri("smtpdnewclient")) == 0 {
		return false
	}

	// serialize message to send
	msg, err := proto.Marshal(&msproto.SmtpdNewClientQuery{
		SessionId: proto.String(s.uuid),
		RemoteIp:  proto.String(s.conn.RemoteAddr().String()),
	})
	if err != nil {
		s.logError("unable to serialize data as SmtpdNewClientMsg. " + err.Error())
		return
	}

	for _, uri := range Cfg.GetMicroservicesUri("smtpdnewclient") {
		stop = false
		ms, err := newMicroservice(uri)
		if err != nil {
			s.logError("unable to parse microservice url " + uri + ". " + err.Error())
			continue
		}

		if s.user != nil && ms.skipAuthentifiedUser {
			continue
		}

		// call ms
		s.log("calling " + ms.url)
		if ms.fireAndForget {
			go ms.call(&msg)
			continue
		}

		response, err := ms.call(&msg)
		if err != nil {
			s.logError("microservice " + ms.url + " failed. " + err.Error())
			if ms.stopOnError() {
				return
			}
			continue
		}

		// parse resp
		msResponse := &msproto.SmtpdNewClientResponse{}
		err = proto.Unmarshal(*response, msResponse)
		if err != nil {
			s.logError("microservice " + ms.url + " failed. " + err.Error())
			if ms.stopOnError() {
				return
			}
			continue
		}

		// send reply (or not)
		stop = handleSMTPResponse(msResponse.GetSmtpResponse(), s)
		// drop ?
		if msResponse.GetDropConnection() {
			s.exitAsap()
			stop = true
		}
		if stop {
			return true
		}
	}
	return
}

// msSmtpdRcptToRelayIsGranted check if relay is granted by using rcpt to
func msSmtpdRcptTo(s *SMTPServerSession, rcptTo string) (stop bool) {
	if len(Cfg.GetMicroservicesUri("smtpdrcptto")) == 0 {
		return false
	}
	msg, err := proto.Marshal(&msproto.SmtpdRcptToQuery{
		SessionId: proto.String(s.uuid),
		Rcptto:    proto.String(rcptTo),
	})
	if err != nil {
		s.logError("unable to serialize data as SmtpdRcptToQuery. " + err.Error())
		return
	}

	for _, uri := range Cfg.GetMicroservicesUri("smtpdrcptto") {
		stop = false
		ms, err := newMicroservice(uri)
		if err != nil {
			s.logError("unable to parse microservice url " + uri + ". " + err.Error())
			continue
		}

		if s.user != nil && ms.skipAuthentifiedUser {
			continue
		}

		// call ms
		s.log("calling " + ms.url)
		if ms.fireAndForget {
			go ms.call(&msg)
			continue
		}

		response, err := ms.call(&msg)
		if err != nil {
			if stop := ms.handleSMTPError(err, s); stop {
				return true
			}
			continue
		}

		// parse resp
		msResponse := &msproto.SmtpdRcptToResponse{}
		err = proto.Unmarshal(*response, msResponse)
		if err != nil {
			if stop := ms.handleSMTPError(err, s); stop {
				return true
			}
			continue
		}

		// Relay granted
		s.relayGranted = msResponse.GetRelayGranted()

		// send reply (or not)
		stop = handleSMTPResponse(msResponse.GetSmtpResponse(), s)
		// drop ?
		if msResponse.GetDropConnection() {
			s.exitAsap()
			stop = true
		}
		if stop {
			return true
		}

	}
	return stop
}

// smtpdData executes microservices for the smtpdData hook
func smtpdData(s *SMTPServerSession, rawMail *[]byte) (stop bool, extraHeaders *[]string) {
	extraHeaders = &[]string{}
	if len(Cfg.GetMicroservicesUri("smtpddata")) == 0 {
		return false, extraHeaders
	}

	// save data to server throught HTTP
	f, err := ioutil.TempFile(Cfg.GetTempDir(), "")
	if err != nil {
		s.logError("ms - unable to save rawmail in tempfile. " + err.Error())
		return false, extraHeaders
	}
	if _, err = f.Write(*rawMail); err != nil {
		s.logError("ms - unable to save rawmail in tempfile. " + err.Error())
		return false, extraHeaders
	}
	defer os.Remove(f.Name())

	// HTTP link
	t := strings.Split(f.Name(), "/")
	link := fmt.Sprintf("%s:%d/msdata/%s", Cfg.GetRestServerIp(), Cfg.GetRestServerPort(), t[len(t)-1])

	// TLS
	if Cfg.GetRestServerIsTls() {
		link = "https://" + link
	} else {
		link = "http://" + link
	}

	// serialize data
	msg, err := proto.Marshal(&msproto.SmtpdDataQuery{
		SessionId: proto.String(s.uuid),
		DataLink:  proto.String(link),
	})
	if err != nil {
		s.logError("unable to serialize data as SmtpdDataQuery. " + err.Error())
		return
	}

	for _, uri := range Cfg.GetMicroservicesUri("smtpddata") {
		// parse uri
		ms, err := newMicroservice(uri)
		if err != nil {
			s.logError("unable to parse microservice url " + uri + ". " + err.Error())
			continue
		}
		if s.user != nil && ms.skipAuthentifiedUser {
			continue
		}

		s.log("calling " + ms.url)
		response, err := ms.call(&msg)
		if err != nil {
			if stop := ms.handleSMTPError(err, s); stop {
				return true, extraHeaders
			}
			continue
		}

		// parse resp
		msResponse := &msproto.SmtpdDataResponse{}
		err = proto.Unmarshal(*response, msResponse)
		if err != nil {
			if stop := ms.handleSMTPError(err, s); stop {
				return true, extraHeaders
			}
			continue
		}

		*extraHeaders = append(*extraHeaders, msResponse.GetExtraHeaders()...)

		// send reply (or not)
		stop = handleSMTPResponse(msResponse.GetSmtpResponse(), s)
		// drop ?
		if msResponse.GetDropConnection() {
			s.exitAsap()
			stop = true
		}
		if stop {
			return true, extraHeaders
		}
	}
	return false, extraHeaders
}

// msGetRoutesmsGetRoutes returns routes from microservices
func msGetRoutes(d *delivery) (routes *[]Route, stop bool) {
	stop = false
	r := []Route{}
	routes = &r
	msURI := Cfg.GetMicroservicesUri("deliverdgetroutes")
	if len(msURI) == 0 {
		return
	}
	// serialize data
	msg, err := proto.Marshal(&msproto.DeliverdGetRoutesQuery{
		DeliverdId:       proto.String(d.id),
		Mailfrom:         proto.String(d.qMsg.MailFrom),
		Rcptto:           proto.String(d.qMsg.RcptTo),
		AuthentifiedUser: proto.String(d.qMsg.AuthUser),
	})

	// There should be only one URI for getroutes
	// so we take msURI[0]
	ms, err := newMicroservice(msURI[0])
	if err != nil {
		//Log.Error("deliverd-ms " + d.id + ": unable to parse microservice url " + msURI[0] + " - " + err.Error())
		Log.Error(fmt.Sprintf("deliverd-remote %s - msGetRoutes - unable to init new ms: %s", d.id, err.Error()))
		return nil, ms.stopOnError()
	}
	Log.Info(fmt.Sprintf("deliverd-remote %s - msGetRoutes - call ms: %s", d.id, ms.url))
	response, err := ms.call(&msg)
	if err != nil {
		Log.Error(fmt.Sprintf("deliverd-remote %s - msGetRoutes - unable to call ms: %s", d.id, err.Error()))
		return nil, ms.stopOnError()
	}

	// parse resp
	msResponse := &msproto.DeliverdGetRoutesResponse{}
	if err := proto.Unmarshal(*response, msResponse); err != nil {
		Log.Error(fmt.Sprintf("deliverd-remote %s - msGetRoutes - unable to unmarshall response: %s", d.id, err.Error()))
		return routes, ms.stopOnError()
	}
	// no routes found
	if len(msResponse.GetRoutes()) == 0 {
		return nil, false
	}
	for _, route := range msResponse.GetRoutes() {
		r := Route{
			RemoteHost: route.GetRemoteHost(),
		}
		if route.GetLocalIp() != "" {
			r.LocalIp = sql.NullString{String: route.GetLocalIp(), Valid: true}
		}
		if route.GetRemotePort() != 0 {
			r.RemotePort = sql.NullInt64{Int64: int64(route.GetRemotePort()), Valid: true}
		}
		if route.GetPriority() != 0 {
			r.Priority = sql.NullInt64{Int64: int64(route.GetPriority()), Valid: true}
		}
		*routes = append(*routes, r)
	}
	return routes, false
}
