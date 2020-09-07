package main

import (
	"fmt"
	"github.com/pkg/errors"
)

const (
	UnexpectedError = iota
	AlreadyStartedError
	NetworkError
	SubcribeError
	UnsubcribeError
	RPCMethodNotFoundError
)

// Standard JSON-RPC 2.0 errors.
var ErrCodeMessage = map[int]struct {
	Code    int
	Message string
}{
	// general
	UnexpectedError:        {-1, "Unexpected error"},
	AlreadyStartedError:    {-2, "RPC server is already started"},
	NetworkError:           {-3, "Network Error"},
	SubcribeError:          {-4, "Failed to subcribe"},
	UnsubcribeError:        {-5, "Failed to unsubcribe"},
	RPCMethodNotFoundError: {-6, "RPCMethod not found"},
}

func NewRPCError(key int, err error, param ...interface{}) *RPCError {
	e := &RPCError{
		Code: ErrCodeMessage[key].Code,
		err:  errors.Wrap(err, ErrCodeMessage[key].Message),
	}
	if len(param) > 0 {
		e.Message = fmt.Sprintf(ErrCodeMessage[key].Message, param)
	} else {
		e.Message = ErrCodeMessage[key].Message
	}
	return e
}

type RPCError struct {
	Code       int    `json:"Code,omitempty"`
	Message    string `json:"Message,omitempty"`
	StackTrace string `json:"StackTrace"`

	err error `json:"Err"`
}

// Error returns a string describing the RPC error.  This satisifies the
// builtin error interface.
func (e RPCError) Error() string {
	return fmt.Sprintf("%d: %+v %+v", e.Code, e.err, e.StackTrace)
}

func (e RPCError) GetErr() error {
	return e.err
}
func GetErrorCode(err int) int {
	return ErrCodeMessage[err].Code
}
