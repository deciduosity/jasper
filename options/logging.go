package options

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/tychoish/birch"
	"github.com/tychoish/grip"
	"github.com/tychoish/grip/level"
	"github.com/tychoish/grip/message"
	"github.com/tychoish/grip/send"
)

// CachedLogger is the cached item representing a processes normal
// output. It captures information about the cached item, as well as
// go interfaces for sending log messages.
type CachedLogger struct {
	ID       string    `bson:"id" json:"id" yaml:"id"`
	Manager  string    `bson:"manager_id" json:"manager_id" yaml:"manager_id"`
	Accessed time.Time `bson:"accessed" json:"accessed" yaml:"accessed"`

	Error  send.Sender `bson:"-" json:"-" yaml:"-"`
	Output send.Sender `bson:"-" json:"-" yaml:"-"`
}

func (cl *CachedLogger) getSender(preferError bool) (send.Sender, error) {
	if preferError && cl.Error != nil {
		return cl.Error, nil
	} else if cl.Output != nil {
		return cl.Output, nil
	} else if cl.Error != nil {
		return cl.Error, nil
	}

	return nil, errors.New("no output configured")
}

// Close closes the underlying output for the cached logger.
func (cl *CachedLogger) Close() error {
	catcher := grip.NewBasicCatcher()
	if cl.Output != nil {
		catcher.Check(cl.Output.Close)
	}

	if cl.Error != nil && cl.Output != cl.Error {
		catcher.Check(cl.Error.Close)
	}
	return catcher.Resolve()
}

// LoggingPayload captures the arguments to the SendMessages operation.
type LoggingPayload struct {
	LoggerID          string               `bson:"logger_id" json:"logger_id" yaml:"logger_id"`
	Data              interface{}          `bson:"data" json:"data" yaml:"data"`
	Priority          level.Priority       `bson:"priority" json:"priority" yaml:"priority"`
	IsMulti           bool                 `bson:"multi,omitempty" json:"multi,omitempty" yaml:"multi,omitempty"`
	PreferSendToError bool                 `bson:"prefer_send_to_error,omitempty" json:"prefer_send_to_error,omitempty" yaml:"prefer_send_to_error,omitempty"`
	AddMetadata       bool                 `bson:"add_metadata,omitempty" json:"add_metadata,omitempty" yaml:"add_metadata,omitempty"`
	Format            LoggingPayloadFormat `bson:"payload_format,omitempty" json:"payload_format,omitempty" yaml:"payload_format,omitempty"`
}

// LoggingPayloadFormat is an set enumerated values describing the
// formating or encoding of the payload data.
type LoggingPayloadFormat string

const (
	LoggingPayloadFormatBSON   = "bson"
	LoggingPayloadFormatJSON   = "json"
	LoggingPayloadFormatSTRING = "string"
)

// Validate checks that the required fields are populated for the payload and
// the format is valid.
func (lp *LoggingPayload) Validate() error {
	catcher := grip.NewBasicCatcher()
	catcher.NewWhen(lp.Data == nil, "data cannot be empty")
	switch lp.Format {
	case "", LoggingPayloadFormatBSON, LoggingPayloadFormatJSON, LoggingPayloadFormatSTRING:
	default:
		catcher.Errorf("invalid payload format '%s'", lp.Format)
	}
	return catcher.Resolve()
}

// Send resolves a sender from the cached logger (either the error or
// output endpoint), and then sends the message from the data
// payload. This method ultimately is responsible for converting the
// payload to a message format.
func (cl *CachedLogger) Send(lp *LoggingPayload) error {
	if err := lp.Validate(); err != nil {
		return errors.Wrap(err, "invalid logging payload")
	}

	sender, err := cl.getSender(lp.PreferSendToError)
	if err != nil {
		return errors.WithStack(err)
	}

	msg, err := lp.convert()
	if err != nil {
		return errors.WithStack(err)
	}

	sender.Send(msg)

	return nil
}

func (lp *LoggingPayload) convert() (message.Composer, error) {
	if lp.IsMulti {
		return lp.convertMultiMessage(lp.Data)
	}
	return lp.convertMessage(lp.Data)
}

func (lp *LoggingPayload) convertMultiMessage(value interface{}) (message.Composer, error) {
	switch data := value.(type) {
	case string:
		return lp.convertMultiMessage(strings.Split(data, "\n"))
	case []byte:
		payload, err := lp.splitByteSlice(data)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return lp.convertMultiMessage(payload)
	case []string:
		batch := []message.Composer{}
		for _, str := range data {
			elem, err := lp.produceMessage([]byte(str))
			if err != nil {
				return nil, errors.WithStack(err)
			}
			batch = append(batch, elem)
		}
		return message.NewGroupComposer(batch), nil
	case [][]byte:
		batch := []message.Composer{}
		for _, dt := range data {
			elem, err := lp.produceMessage(dt)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			batch = append(batch, elem)
		}
		return message.NewGroupComposer(batch), nil
	case []interface{}:
		batch := []message.Composer{}
		for _, dt := range data {
			elem, err := lp.convertMessage(dt)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			batch = append(batch, elem)
		}
		return message.NewGroupComposer(batch), nil
	default:
		return message.ConvertToComposer(lp.Priority, value), nil
	}
}

func (lp *LoggingPayload) convertMessage(value interface{}) (message.Composer, error) {
	switch data := value.(type) {
	case string:
		return lp.produceMessage([]byte(data))
	case []byte:
		return lp.produceMessage(data)
	case []string:
		return message.ConvertToComposer(lp.Priority, data), nil
	case [][]byte:
		return message.NewLineMessage(lp.Priority, byteSlicesToStringSlice(data)), nil
	case message.Fields:
		if lp.AddMetadata {
			return message.NewFields(lp.Priority, data), nil
		}
		return message.NewSimpleFields(lp.Priority, data), nil
	case []message.Fields:
		msgs := make([]message.Composer, len(data))
		for idx := range data {
			if lp.AddMetadata {
				msgs[idx] = message.NewFields(lp.Priority, data[idx])
			} else {
				msgs[idx] = message.NewSimpleFields(lp.Priority, data[idx])
			}
		}

		return message.NewGroupComposer(msgs), nil
	case []interface{}:
		return message.NewLineMessage(lp.Priority, data...), nil
	default:
		return message.ConvertToComposer(lp.Priority, value), nil
	}
}

func (lp *LoggingPayload) produceMessage(data []byte) (message.Composer, error) {
	switch lp.Format {
	case LoggingPayloadFormatJSON:
		payload := message.Fields{}
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, errors.Wrap(err, "problem parsing json from message body")
		}

		if lp.AddMetadata {
			return message.NewFields(lp.Priority, payload), nil
		}

		return message.NewSimpleFields(lp.Priority, payload), nil
	case LoggingPayloadFormatBSON:
		unmarshler := GetGlobalLoggerRegistry().Unmarshaler(RawLoggerConfigFormatBSON)
		if unmarshler == nil {
			return nil, errors.New("no suitable unmarshaller provided")
		}

		payload := message.Fields{}
		if err := unmarshler(data, &payload); err != nil {
			return nil, errors.Wrap(err, "problem parsing bson from message body")
		}
		if lp.AddMetadata {
			return message.NewFields(lp.Priority, payload), nil
		}

		return message.NewSimpleFields(lp.Priority, payload), nil
	default: // includes string case.
		if lp.AddMetadata {
			return message.NewBytesMessage(lp.Priority, data), nil
		}

		return message.NewSimpleBytesMessage(lp.Priority, data), nil
	}
}

func (lp *LoggingPayload) splitByteSlice(data []byte) (interface{}, error) {
	if lp.Format != LoggingPayloadFormatBSON {
		return bytes.Split(data, []byte("\x00")), nil
	}

	out := [][]byte{}
	buf := bytes.NewBuffer(data)
	for {
		doc := birch.DC.New()
		_, err := doc.ReadFrom(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrap(err, "problem reading bson from message data")
		}

		payload, err := doc.MarshalBSON()
		if err != nil {
			return nil, errors.Wrap(err, "problem constructing bson form")
		}
		out = append(out, payload)
	}

	return out, nil
}

func byteSlicesToStringSlice(in [][]byte) []string {
	out := make([]string, len(in))
	for idx := range in {
		out[idx] = string(in[idx])
	}
	return out
}
