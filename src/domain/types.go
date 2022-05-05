package domain

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueformat "cuelang.org/go/cue/format"
	cueliteral "cuelang.org/go/cue/literal"
	"cuelang.org/go/tools/flow"
	"github.com/google/uuid"
	nomad "github.com/hashicorp/nomad/api"
	"github.com/pkg/errors"

	"github.com/input-output-hk/cicero/src/util"
)

type InputDefinitionMatch string

// There is a race condition around global internal state of CUE.
var cueMutex = &sync.Mutex{}

func (self *InputDefinitionMatch) WithInputs(inputs map[string]*Fact) cue.Value {
	cueMutex.Lock()
	defer cueMutex.Unlock()

	ctx := cuecontext.New()
	return ctx.CompileString(
		string(*self),
		cue.Scope(ctx.Encode(struct{}{}).
			// XXX check which inputs are actually used and pass in only those
			FillPath(cue.MakePath(cue.Hid("_inputs", "_")), inputs),
		),
	)
}

func (self *InputDefinitionMatch) WithoutInputs() cue.Value {
	return self.WithInputs(map[string]*Fact{})
}

func (self *InputDefinitionMatch) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}

	match := InputDefinitionMatch(str)
	if err := match.WithoutInputs().Err(); err != nil {
		return err
	}

	*self = match
	return nil
}

// Makes sure to be valid CUE that does not evaluate to an error
// by marshaling the parsed syntax instead of the raw string.
func (self InputDefinitionMatch) MarshalJSON() ([]byte, error) {
	match := self.WithoutInputs()
	if err := match.Err(); err != nil {
		return nil, err
	} else if syntax, err := cueformat.Node(
		match.Syntax(
			cue.Hidden(true),
			cue.Optional(true),
			cue.ResolveReferences(false),
		),
		cueformat.Simplify(),
	); err != nil {
		return nil, err
	} else {
		return json.Marshal(string(syntax))
	}
}

func (self *InputDefinitionMatch) Scan(value interface{}) error {
	return self.UnmarshalJSON(value.([]byte))
}

type InputDefinition struct {
	Not      bool                 `json:"not"`
	Optional bool                 `json:"optional"`
	Match    InputDefinitionMatch `json:"match"`
}

type InputDefinitions map[string]InputDefinition

func (self *InputDefinitions) Flow(runnerFunc flow.RunnerFunc) *flow.Controller {
	cueMutex.Lock()
	defer cueMutex.Unlock()

	cueStr := ``
	for name, input := range *self {
		cueStr += `_inputs: `
		cueStr = string(cueliteral.Label.Append([]byte(cueStr), name))
		cueStr += `: value: {` + string(input.Match) + "}\n"
	}
	value := cuecontext.New().CompileString(cueStr)

	return flow.New(
		&flow.Config{Root: cue.MakePath(cue.Hid("_inputs", "_"))},
		value,
		func(v cue.Value) (flow.Runner, error) {
			if len(v.Path().Selectors()) != 2 {
				return nil, nil
			}
			return runnerFunc, nil
		},
	)
}

type ActionDefinition struct {
	Meta   map[string]interface{} `json:"meta"`
	Inputs InputDefinitions       `json:"inputs"`
}

type RunOutput struct {
	Failure *interface{} `json:"failure"`
	Success *interface{} `json:"success"`
}

type RunDefinition struct {
	Output RunOutput  `json:"output"`
	Job    *nomad.Job `json:"job"`
}

func (s *RunDefinition) IsDecision() bool {
	return s.Job == nil
}

type Fact struct {
	ID         uuid.UUID   `json:"id"`
	RunId      *uuid.UUID  `json:"run_id,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	Value      interface{} `json:"value"`
	BinaryHash *string     `json:"binary_hash,omitempty"`
	// TODO nyi: unique key over (value, binary_hash)?
}

// Sets the value from JSON and returns the rest of the buffer as binary.
func (f *Fact) FromReader(reader io.Reader, trimWhitespace bool) (io.Reader, error) {
	factDecoder := json.NewDecoder(reader)
	if err := factDecoder.Decode(&f.Value); err != nil {
		return nil, errors.WithMessage(err, "Could not unmarshal json body")
	} else {
		binary := io.MultiReader(factDecoder.Buffered(), reader)
		if trimWhitespace {
			binary = util.SkipLeadingWhitespaceReader(binary)
		}
		return binary, nil
	}
}

type Action struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	Active    bool      `json:"active"`
	ActionDefinition
}

type Run struct {
	NomadJobID   uuid.UUID  `json:"nomad_job_id"`
	InvocationId uuid.UUID  `json:"invocation_id"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

type Invocation struct {
	Id         uuid.UUID `json:"id"`
	ActionId   uuid.UUID `json:"action_id"`
	CreatedAt  time.Time `json:"created_at"`
	EvalStdout *string   `json:"eval_stdout"`
	EvalStderr *string   `json:"eval_stderr"`
}

type NomadEvent struct {
	nomad.Event
	Uid     MD5Sum
	Handled bool
}

type MD5Sum [16]byte

func (self *MD5Sum) Scan(value interface{}) error {
	if b, ok := value.([]byte); !ok {
		return fmt.Errorf("Cannot scan %T into MD5Sum", value)
	} else if copied := copy(self[:], b); copied != len(*self) {
		return fmt.Errorf("Could only copy %d/%d bytes into MD5Sum", copied, len(*self))
	}
	return nil
}
