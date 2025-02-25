// Package jsonrpc implements a JSONRPC2.0 compliant server as described in https://www.jsonrpc.org/specification
package jsonrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/NethermindEth/juno/utils"
	"github.com/sourcegraph/conc/pool"
)

const (
	InvalidJSON    = -32700 // Invalid JSON was received by the server.
	InvalidRequest = -32600 // The JSON sent is not a valid Request object.
	MethodNotFound = -32601 // The method does not exist / is not available.
	InvalidParams  = -32602 // Invalid method parameter(s).
	InternalError  = -32603 // Internal JSON-RPC error.
)

var (
	ErrInvalidID = errors.New("id should be a string or an integer")

	bufferSize       = 128
	contextInterface = reflect.TypeOf((*context.Context)(nil)).Elem()
)

type request struct {
	Version string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      any    `json:"id,omitempty"`
}

type response struct {
	Version string `json:"jsonrpc"`
	Result  any    `json:"result,omitempty"`
	Error   *Error `json:"error,omitempty"`
	ID      any    `json:"id"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func Err(code int, data any) *Error {
	switch code {
	case InvalidJSON:
		return &Error{Code: InvalidJSON, Message: "Parse error", Data: data}
	case InvalidRequest:
		return &Error{Code: InvalidRequest, Message: "Invalid Request", Data: data}
	case MethodNotFound:
		return &Error{Code: MethodNotFound, Message: "Method Not Found", Data: data}
	case InvalidParams:
		return &Error{Code: InvalidParams, Message: "Invalid Params", Data: data}
	default:
		return &Error{Code: InternalError, Message: "Internal Error", Data: data}
	}
}

func (r *request) isSane() error {
	if r.Version != "2.0" {
		return errors.New("unsupported RPC request version")
	}
	if r.Method == "" {
		return errors.New("no method specified")
	}

	if r.Params != nil {
		paramType := reflect.TypeOf(r.Params)
		if paramType.Kind() != reflect.Slice && paramType.Kind() != reflect.Map {
			return errors.New("params should be an array or an object")
		}
	}

	if r.ID != nil {
		idType := reflect.TypeOf(r.ID)
		floating := idType.Name() == "Number" && strings.Contains(r.ID.(json.Number).String(), ".")
		if (idType.Kind() != reflect.String && idType.Name() != "Number") || floating {
			return ErrInvalidID
		}
	}

	return nil
}

type Parameter struct {
	Name     string
	Optional bool
}

type Method struct {
	Name    string
	Params  []Parameter
	Handler any

	// The method takes a context as its first parameter.
	// Set upon successful registration.
	needsContext bool
}

type Server struct {
	methods   map[string]Method
	validator Validator
	pool      *pool.Pool
	log       utils.SimpleLogger
	listener  EventListener
}

type Validator interface {
	Struct(any) error
}

// NewServer instantiates a JSONRPC server
func NewServer(poolMaxGoroutines int, log utils.SimpleLogger) *Server {
	s := &Server{
		log:      log,
		methods:  make(map[string]Method),
		pool:     pool.New().WithMaxGoroutines(poolMaxGoroutines),
		listener: &SelectiveListener{},
	}

	return s
}

// WithValidator registers a validator to validate handler struct arguments
func (s *Server) WithValidator(validator Validator) *Server {
	s.validator = validator
	return s
}

// WithListener registers an EventListener
func (s *Server) WithListener(listener EventListener) *Server {
	s.listener = listener
	return s
}

// RegisterMethods verifies and creates an endpoint that the server recognises.
//
// - name is the method name
// - handler is the function to be called when a request is received for the
// associated method. It should have (any, *jsonrpc.Error) as its return type
// - paramNames are the names of parameters in the order that they are expected
// by the handler
func (s *Server) RegisterMethods(methods ...Method) error {
	for idx := range methods {
		if err := s.registerMethod(methods[idx]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) registerMethod(method Method) error {
	handlerT := reflect.TypeOf(method.Handler)
	if handlerT.Kind() != reflect.Func {
		return errors.New("handler must be a function")
	}
	numArgs := handlerT.NumIn()
	if numArgs > 0 {
		if handlerT.In(0).Implements(contextInterface) {
			numArgs--
			method.needsContext = true
		}
	}
	if numArgs != len(method.Params) {
		return errors.New("number of non-context function params and param names must match")
	}
	if handlerT.NumOut() != 2 {
		return errors.New("handler must return 2 values")
	}
	if handlerT.Out(1) != reflect.TypeOf(&Error{}) {
		return errors.New("second return value must be a *jsonrpc.Error")
	}

	// The method is valid. Mutate the appropriate fields and register on the server.
	s.methods[method.Name] = method

	return nil
}

// Handle processes a request to the server
// It returns the response in a byte array, only returns an
// error if it can not create the response byte array
func (s *Server) Handle(ctx context.Context, data []byte) ([]byte, error) {
	return s.HandleReader(ctx, bytes.NewReader(data))
}

type ConnKey struct{}

func ConnFromContext(ctx context.Context) (io.Writer, bool) {
	conn := ctx.Value(ConnKey{})
	if conn == nil {
		return nil, false
	}
	w, ok := conn.(io.Writer)
	return w, ok
}

// HandleReader processes a request to the server
// It returns the response in a byte array, only returns an
// error if it can not create the response byte array
func (s *Server) HandleReader(ctx context.Context, reader io.Reader) ([]byte, error) {
	bufferedReader := bufio.NewReaderSize(reader, bufferSize)
	requestIsBatch := isBatch(bufferedReader)
	res := &response{
		Version: "2.0",
	}

	dec := json.NewDecoder(bufferedReader)
	dec.UseNumber()

	if !requestIsBatch {
		req := new(request)
		if jsonErr := dec.Decode(req); jsonErr != nil {
			res.Error = Err(InvalidJSON, jsonErr.Error())
		} else if resObject, handleErr := s.handleRequest(ctx, req); handleErr != nil {
			if !errors.Is(handleErr, ErrInvalidID) {
				res.ID = req.ID
			}
			res.Error = Err(InvalidRequest, handleErr.Error())
		} else {
			res = resObject
		}
	} else {
		var batchReq []json.RawMessage

		if batchJSONErr := dec.Decode(&batchReq); batchJSONErr != nil {
			res.Error = Err(InvalidJSON, batchJSONErr.Error())
		} else if len(batchReq) == 0 {
			res.Error = Err(InvalidRequest, "empty batch")
		} else {
			return s.handleBatchRequest(ctx, batchReq)
		}
	}

	if res == nil {
		return nil, nil
	}
	return json.Marshal(res)
}

func (s *Server) handleBatchRequest(ctx context.Context, batchReq []json.RawMessage) ([]byte, error) {
	var (
		responses []json.RawMessage
		mutex     sync.Mutex
	)

	addResponse := func(response any) {
		if responseJSON, err := json.Marshal(response); err != nil {
			s.log.Errorw("failed to marshal response", "err", err)
		} else {
			mutex.Lock()
			responses = append(responses, responseJSON)
			mutex.Unlock()
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for _, rawReq := range batchReq {
		reqDec := json.NewDecoder(bytes.NewBuffer(rawReq))
		reqDec.UseNumber()

		req := new(request)
		if err := reqDec.Decode(req); err != nil {
			addResponse(&response{
				Version: "2.0",
				Error:   Err(InvalidRequest, err.Error()),
			})
			continue
		}

		wg.Add(1)
		s.pool.Go(func() {
			defer wg.Done()

			resp, err := s.handleRequest(ctx, req)
			if err != nil {
				resp = &response{
					Version: "2.0",
					Error:   Err(InvalidRequest, err.Error()),
				}
				if !errors.Is(err, ErrInvalidID) {
					resp.ID = req.ID
				}
			}
			// for notification request response is nil
			if resp != nil {
				addResponse(resp)
			}
		})
	}

	wg.Wait()
	// according to the spec if there are no response objects server must not return empty array
	if len(responses) == 0 {
		return nil, nil
	}

	return json.Marshal(responses)
}

func isBatch(reader *bufio.Reader) bool {
	for {
		char, err := reader.Peek(1)
		if err != nil {
			break
		}
		if char[0] == ' ' || char[0] == '\t' || char[0] == '\r' || char[0] == '\n' {
			if discarded, err := reader.Discard(1); discarded != 1 || err != nil {
				break
			}
			continue
		}
		return char[0] == '['
	}
	return false
}

func isNil(i any) bool {
	return i == nil || reflect.ValueOf(i).IsNil()
}

func (s *Server) handleRequest(ctx context.Context, req *request) (*response, error) {
	reqJSON, err := json.Marshal(req)
	if err == nil {
		s.log.Debugw("Serving RPC request", "request", string(reqJSON))
	}

	if err = req.isSane(); err != nil {
		return nil, err
	}

	res := &response{
		Version: "2.0",
		ID:      req.ID,
	}

	calledMethod, found := s.methods[req.Method]
	if !found {
		res.Error = Err(MethodNotFound, nil)
		return res, nil
	}

	handlerTimer := time.Now()
	s.listener.OnNewRequest(req.Method)
	args, err := s.buildArguments(ctx, req.Params, calledMethod)
	if err != nil {
		res.Error = Err(InvalidParams, err.Error())
		s.listener.OnRequestFailed(req.Method, err)
		return res, nil
	}
	defer func() {
		handlerTook := time.Since(handlerTimer)
		s.listener.OnRequestHandled(req.Method, handlerTook)
		s.log.Debugw("Responding to RPC request", "method", req.Method, "id", req.ID, "took", handlerTook)
	}()

	tuple := reflect.ValueOf(calledMethod.Handler).Call(args)
	if res.ID == nil { // notification
		return nil, nil
	}

	if errAny := tuple[1].Interface(); !isNil(errAny) {
		res.Error = errAny.(*Error)
		s.listener.OnRequestFailed(req.Method, err)
		return res, nil
	}
	res.Result = tuple[0].Interface()
	return res, nil
}

func (s *Server) buildArguments(ctx context.Context, params any, method Method) ([]reflect.Value, error) {
	handlerType := reflect.TypeOf(method.Handler)

	numArgs := handlerType.NumIn()
	args := make([]reflect.Value, 0, numArgs)
	addContext := 0

	if method.needsContext {
		args = append(args, reflect.ValueOf(ctx))
		addContext = 1
	}

	if isNil(params) {
		allParamsAreOptional := utils.All(method.Params, func(p Parameter) bool {
			return p.Optional
		})

		if len(method.Params) > 0 && !allParamsAreOptional {
			return nil, errors.New("missing non-optional param field")
		}

		for i := addContext; i < numArgs; i++ {
			arg := reflect.New(handlerType.In(i)).Elem()
			args = append(args, arg)
		}

		return args, nil
	}

	switch reflect.TypeOf(params).Kind() {
	case reflect.Slice:
		paramsList := params.([]any)

		if len(paramsList) != numArgs-addContext {
			return nil, errors.New("missing/unexpected params in list")
		}

		for i, param := range paramsList {
			v, err := s.parseParam(param, handlerType.In(i+addContext))
			if err != nil {
				return nil, err
			}
			args = append(args, v)
		}
	case reflect.Map:
		paramsMap := params.(map[string]any)

		for i, configuredParam := range method.Params {
			var v reflect.Value
			if param, found := paramsMap[configuredParam.Name]; found {
				var err error
				v, err = s.parseParam(param, handlerType.In(i+addContext))
				if err != nil {
					return nil, err
				}
			} else if configuredParam.Optional {
				// optional parameter
				v = reflect.New(handlerType.In(i + addContext)).Elem()
			} else {
				return nil, errors.New("missing non-optional param")
			}

			args = append(args, v)
		}
	default:
		// Todo: consider returning InternalError
		return nil, errors.New("impossible param type: check request.isSane")
	}
	return args, nil
}

func (s *Server) parseParam(param any, t reflect.Type) (reflect.Value, error) {
	handlerParam := reflect.New(t)
	valueMarshaled, err := json.Marshal(param) // we have to marshal the value into JSON again
	if err != nil {
		return reflect.ValueOf(nil), err
	}
	err = json.Unmarshal(valueMarshaled, handlerParam.Interface())
	if err != nil {
		return reflect.ValueOf(nil), err
	}

	elem := handlerParam.Elem()
	if s.validator != nil {
		if err = s.validateParam(elem); err != nil {
			return reflect.ValueOf(nil), err
		}
	}

	return elem, nil
}

func (s *Server) validateParam(param reflect.Value) error {
	kind := param.Kind()
	switch {
	case kind == reflect.Struct ||
		(kind == reflect.Pointer && param.Elem().Kind() == reflect.Struct):
		/* struct or a struct pointer */
		if err := s.validator.Struct(param.Interface()); err != nil {
			return err
		}
	case kind == reflect.Slice || kind == reflect.Array:
		for i := 0; i < param.Len(); i++ {
			if err := s.validateParam(param.Index(i)); err != nil {
				return err
			}
		}
	case kind == reflect.Map:
		for _, key := range param.MapKeys() {
			if err := s.validateParam(param.MapIndex(key)); err != nil {
				return err
			}
		}
	}

	return nil
}
