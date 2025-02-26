package blademaster

import (
	"context"
	"fmt"
	"github.com/go-playground/validator/v10"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"kratos/pkg/net/metadata"

	"kratos/pkg/ecode"
	"kratos/pkg/net/http/blademaster/binding"
	"kratos/pkg/net/http/blademaster/render"

	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
)

const (
	_abortIndex int8 = math.MaxInt8 / 2
)

var (
	_openParen  = []byte("(")
	_closeParen = []byte(")")
)

// Context is the most important part. It allows us to pass variables between
// middleware, manage the flow, validate the JSON of a request and render a
// JSON response for example.
type Context struct {
	context.Context

	Request *http.Request
	Writer  http.ResponseWriter

	// flow control
	index    int8
	handlers []HandlerFunc

	// Keys is a key/value pair exclusively for the context of each request.
	Keys map[string]interface{}
	// This mutex protect Keys map
	keysMutex sync.RWMutex

	Error    error
	ErrorMsg string

	method string
	engine *Engine

	RoutePath string

	Params Params
}

/************************************/
/********** CONTEXT CREATION ********/
/************************************/
func (c *Context) reset() {
	c.Context = nil
	c.index = -1
	c.handlers = nil
	c.Keys = nil
	c.Error = nil
	c.ErrorMsg = ""
	c.method = ""
	c.RoutePath = ""
	c.Params = c.Params[0:0]
}

/************************************/
/*********** FLOW CONTROL ***********/
/************************************/

// Next should be used only inside middleware.
// It executes the pending handlers in the chain inside the calling handler.
// See example in godoc.
func (c *Context) Next() {
	c.index++
	for c.index < int8(len(c.handlers)) {
		c.handlers[c.index](c)
		c.index++
	}
}

// Abort prevents pending handlers from being called. Note that this will not stop the current handler.
// Let's say you have an authorization middleware that validates that the current request is authorized.
// If the authorization fails (ex: the password does not match), call Abort to ensure the remaining handlers
// for this request are not called.
func (c *Context) Abort() {
	c.index = _abortIndex
}

// AbortWithStatus calls `Abort()` and writes the headers with the specified status code.
// For example, a failed attempt to authenticate a request could use: context.AbortWithStatus(401).
func (c *Context) AbortWithStatus(code int) {
	c.Status(code)
	c.Abort()
}

// IsAborted returns true if the current context was aborted.
func (c *Context) IsAborted() bool {
	return c.index >= _abortIndex
}

/************************************/
/******** METADATA MANAGEMENT********/
/************************************/

// Set is used to store a new key/value pair exclusively for this context.
// It also lazy initializes  c.Keys if it was not used previously.
func (c *Context) Set(key string, value interface{}) {
	c.keysMutex.Lock()
	if c.Keys == nil {
		c.Keys = make(map[string]interface{})
	}
	c.Keys[key] = value
	c.keysMutex.Unlock()
}

// Get returns the value for the given key, ie: (value, true).
// If the value does not exists it returns (nil, false)
func (c *Context) Get(key string) (value interface{}, exists bool) {
	c.keysMutex.RLock()
	value, exists = c.Keys[key]
	c.keysMutex.RUnlock()
	return
}

// GetString returns the value associated with the key as a string.
func (c *Context) GetString(key string) (s string) {
	if val, ok := c.Get(key); ok && val != nil {
		s, _ = val.(string)
	}
	return
}

// GetBool returns the value associated with the key as a boolean.
func (c *Context) GetBool(key string) (b bool) {
	if val, ok := c.Get(key); ok && val != nil {
		b, _ = val.(bool)
	}
	return
}

// GetInt returns the value associated with the key as an integer.
func (c *Context) GetInt(key string) (i int) {
	if val, ok := c.Get(key); ok && val != nil {
		i, _ = val.(int)
	}
	return
}

// GetUint returns the value associated with the key as an unsigned integer.
func (c *Context) GetUint(key string) (ui uint) {
	if val, ok := c.Get(key); ok && val != nil {
		ui, _ = val.(uint)
	}
	return
}

// GetInt64 returns the value associated with the key as an integer.
func (c *Context) GetInt64(key string) (i64 int64) {
	if val, ok := c.Get(key); ok && val != nil {
		i64, _ = val.(int64)
	}
	return
}

// GetUint64 returns the value associated with the key as an unsigned integer.
func (c *Context) GetUint64(key string) (ui64 uint64) {
	if val, ok := c.Get(key); ok && val != nil {
		ui64, _ = val.(uint64)
	}
	return
}

// GetFloat64 returns the value associated with the key as a float64.
func (c *Context) GetFloat64(key string) (f64 float64) {
	if val, ok := c.Get(key); ok && val != nil {
		f64, _ = val.(float64)
	}
	return
}

/************************************/
/******** RESPONSE RENDERING ********/
/************************************/

// bodyAllowedForStatus is a copy of http.bodyAllowedForStatus non-exported function.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}
	return true
}

// Status sets the HTTP response code.
func (c *Context) Status(code int) {
	c.Writer.WriteHeader(code)
}

// Render http response with http code by a render instance.
func (c *Context) Render(code int, r render.Render) {
	r.WriteContentType(c.Writer)
	if code > 0 {
		c.Status(code)
	}

	if !bodyAllowedForStatus(code) {
		return
	}

	params := c.Request.Form
	cb := template.JSEscapeString(params.Get("callback"))
	jsonp := cb != ""
	if jsonp {
		c.Writer.Write([]byte(cb))
		c.Writer.Write(_openParen)
	}

	if err := r.Render(c.Writer); err != nil {
		c.Error = err
		return
	}

	if jsonp {
		if _, err := c.Writer.Write(_closeParen); err != nil {
			c.Error = errors.WithStack(err)
		}
	}
}

// JSON serializes the given struct as JSON into the response body.
// It also sets the Content-Type as "application/json".
func (c *Context) JSON(data interface{}, err error) {
	code := http.StatusOK
	c.Error = err
	bcode := ecode.Cause(err)
	// TODO app allow 5xx?
	/*
		if bcode.Code() == -500 {
			code = http.StatusServiceUnavailable
		}
	*/
	writeStatusCode(c.Writer, bcode.Code())
	// 历史项目中需要兼容返回code message 和其他返回内容时，快速稳定兼容。不需要调整业务代码
	cc := strings.Split(bcode.Message(), "||")
	if len(cc) > 1 { // 兼容逻辑
		out := make(map[string]interface{})
		out["ret"] = bcode.Code()
		out["message"] = cc[1]
		out["code"] = cc[0]
		out["now"] = time.Now().Unix()
		out["data"] = data
		c.Render(code, render.MapJSON(out))

	} else {
		c.Render(code, render.JSON{
			Code:    bcode.Code(),
			Message: bcode.Message(),
			Data:    data,
		})
	}
}

// JSONMap serializes the given map as map JSON into the response body.
// It also sets the Content-Type as "application/json".
func (c *Context) JSONMap(data map[string]interface{}, err error) {
	code := http.StatusOK
	c.Error = err
	bcode := ecode.Cause(err)
	// TODO app allow 5xx?
	/*
		if bcode.Code() == -500 {
			code = http.StatusServiceUnavailable
		}
	*/
	writeStatusCode(c.Writer, bcode.Code())
	data["code"] = bcode.Code()
	if _, ok := data["message"]; !ok {
		data["message"] = bcode.Message()
	}
	c.Render(code, render.MapJSON(data))
}

// XML serializes the given struct as XML into the response body.
// It also sets the Content-Type as "application/xml".
func (c *Context) XML(data interface{}, err error) {
	code := http.StatusOK
	c.Error = err
	bcode := ecode.Cause(err)
	// TODO app allow 5xx?
	/*
		if bcode.Code() == -500 {
			code = http.StatusServiceUnavailable
		}
	*/
	writeStatusCode(c.Writer, bcode.Code())
	c.Render(code, render.XML{
		Code:    bcode.Code(),
		Message: bcode.Message(),
		Data:    data,
	})
}

// Protobuf serializes the given struct as PB into the response body.
// It also sets the ContentType as "application/x-protobuf".
func (c *Context) Protobuf(data proto.Message, err error) {
	var (
		bytes []byte
	)

	code := http.StatusOK
	c.Error = err
	bcode := ecode.Cause(err)

	any := new(types.Any)
	if data != nil {
		if bytes, err = proto.Marshal(data); err != nil {
			c.Error = errors.WithStack(err)
			return
		}
		any.TypeUrl = "type.googleapis.com/" + proto.MessageName(data)
		any.Value = bytes
	}
	writeStatusCode(c.Writer, bcode.Code())
	c.Render(code, render.PB{
		Code:    int64(bcode.Code()),
		Message: bcode.Message(),
		Data:    any,
	})
}

// Bytes writes some data into the body stream and updates the HTTP code.
func (c *Context) Bytes(code int, contentType string, data ...[]byte) {
	c.Render(code, render.Data{
		ContentType: contentType,
		Data:        data,
	})
}

// String writes the given string into the response body.
func (c *Context) String(code int, format string, values ...interface{}) {
	c.Render(code, render.String{Format: format, Data: values})
}

// Redirect returns a HTTP redirect to the specific location.
func (c *Context) Redirect(code int, location string) {
	c.Render(-1, render.Redirect{
		Code:     code,
		Location: location,
		Request:  c.Request,
	})
}

// BindWith bind req arg with parser.
func (c *Context) BindWith(obj interface{}, b binding.Binding) error {
	return c.mustBindWith(obj, b)
}

// Bind checks the Content-Type to select a binding engine automatically,
// Depending the "Content-Type" header different bindings are used:
//     "application/json" --> JSON binding
//     "application/xml"  --> XML binding
// otherwise --> returns an error.
// It parses the request's body as JSON if Content-Type == "application/json" using JSON or XML as a JSON input.
// It decodes the json payload into the struct specified as a pointer.
// It writes a 400 error and sets Content-Type header "text/plain" in the response if input is not valid.
func (c *Context) Bind(obj interface{}) error {
	b := binding.Default(c.Request.Method, c.Request.Header.Get("Content-Type"))
	return c.mustBindWith(obj, b)
}

// mustBindWith binds the passed struct pointer using the specified binding engine.
// It will abort the request with HTTP 400 if any error ocurrs.
// See the binding package.
func (c *Context) mustBindWith(obj interface{}, b binding.Binding) (err error) {
	if err = b.Bind(c.Request, obj); err != nil {
		c.Error = ecode.RequestErr
		c.ErrorMsg = err.Error()
		errs, ok := err.(validator.ValidationErrors)
		var message string
		if !ok {
			message = err.Error()
		} else {
			for _, v := range errs.Translate(binding.Validator.GetTranslator()) {
				message += fmt.Sprintf("%s;", v)
			}
		}
		c.Render(http.StatusOK, render.JSON{
			Code:    ecode.RequestErr.Code(),
			Message: message,
			Data:    nil,
		})
		c.Abort()
	}
	return
}

func writeStatusCode(w http.ResponseWriter, ecode int) {
	header := w.Header()
	header.Set("kratos-status-code", strconv.FormatInt(int64(ecode), 10))
}

// RemoteIP implements a best effort algorithm to return the real client IP, it parses
// X-Real-IP and X-Forwarded-For in order to work properly with reverse-proxies such us: nginx or haproxy.
// Use X-Forwarded-For before X-Real-Ip as nginx uses X-Real-Ip with the proxy's IP.
// Notice: metadata.RemoteIP take precedence over X-Forwarded-For and X-Real-Ip
func (c *Context) RemoteIP() (remoteIP string) {
	remoteIP = metadata.String(c, metadata.RemoteIP)
	if remoteIP != "" {
		return
	}

	remoteIP = c.Request.Header.Get("X-Forwarded-For")
	remoteIP = strings.TrimSpace(strings.Split(remoteIP, ",")[0])
	if remoteIP == "" {
		remoteIP = strings.TrimSpace(c.Request.Header.Get("X-Real-Ip"))
	}

	return
}
