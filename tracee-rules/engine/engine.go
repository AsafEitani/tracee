package engine

import (
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"

	tracee "github.com/aquasecurity/tracee/tracee-ebpf/external"
	"github.com/aquasecurity/tracee/tracee-rules/types"
	"github.com/open-policy-agent/opa/ast"
)

const ALL_EVENT_ORIGINS = "*"
const EVENT_CONTAINER_ORIGIN = "container"
const EVENT_HOST_ORIGIN = "host"
const ALL_EVENT_TYPES = "*"

// Engine is a rule-engine that can process events coming from a set of input sources against a set of loaded signatures, and report the signatures' findings
type Engine struct {
	logger          log.Logger
	signatures      map[types.Signature]chan types.Event
	signaturesIndex map[types.SignatureEventSelector][]types.Signature
	signaturesMutex sync.RWMutex
	inputs          EventSources
	output          chan types.Finding
	waitGroup       *sync.WaitGroup
}

//EventSources is a bundle of input sources used to configure the Engine
type EventSources struct {
	Tracee chan types.Event
}

// NewEngine creates a new rules-engine with the given arguments
// inputs and outputs are given as channels created by the consumer
func NewEngine(sigs []types.Signature, sources EventSources, output chan types.Finding, logWriter io.Writer, wg *sync.WaitGroup) (*Engine, error) {
	if sources.Tracee == nil || output == nil || logWriter == nil {
		return nil, fmt.Errorf("nil input received")
	}
	engine := Engine{}
	if wg == nil {
		wg = &sync.WaitGroup{}
	}
	engine.waitGroup = wg
	engine.logger = *log.New(logWriter, "", 0)
	engine.inputs = sources
	engine.output = output
	engine.signaturesMutex.Lock()
	engine.signatures = make(map[types.Signature]chan types.Event)
	engine.signaturesIndex = make(map[types.SignatureEventSelector][]types.Signature)
	engine.signaturesMutex.Unlock()
	for _, sig := range sigs {
		engine.signaturesMutex.Lock()
		engine.signatures[sig] = make(chan types.Event)
		engine.signaturesMutex.Unlock()
		meta, err := sig.GetMetadata()
		if err != nil {
			engine.logger.Printf("error getting metadata: %v", err)
			continue
		}
		se, err := sig.GetSelectedEvents()
		if err != nil {
			engine.logger.Printf("error getting selected events for signature %s: %v", meta.Name, err)
			continue
		}
		for _, es := range se {
			if es.Name == "" {
				es.Name = ALL_EVENT_TYPES
			}
			if es.Origin == "" {
				es.Origin = ALL_EVENT_ORIGINS
			}
			if es.Source == "" {
				engine.logger.Printf("signature %s doesn't declare an input source", meta.Name)
			} else {
				engine.signaturesMutex.Lock()
				engine.signaturesIndex[es] = append(engine.signaturesIndex[es], sig)
				engine.signaturesMutex.Unlock()
			}
		}
		err = sig.Init(engine.matchHandler)
		if err != nil {
			engine.logger.Printf("error initializing signature %s: %v", meta.Name, err)
			continue
		}
	}
	engine.signaturesMutex.RLock()
	lenSigs := len(engine.signatures)
	engine.signaturesMutex.RUnlock()
	if len(sigs) != lenSigs {
		return nil, errors.New("one or more signatures are not uniquely identifiable")
	}
	return &engine, nil
}

// signatureStart is the signature handling business logics.
func signatureStart(signature types.Signature, c chan types.Event, wg *sync.WaitGroup) {
	wg.Add(1)
	for e := range c {
		if err := signature.OnEvent(e); err != nil {
			meta, _ := signature.GetMetadata()
			log.Printf("error handling event by signature %s: %v", meta.Name, err)
		}
	}
	wg.Done()
}

// Start starts processing events and detecting signatures
// it runs continuously until stopped by the done channel
// once done, it cleans all internal resources, which means the engine is not reusable
// note that the input and output channels are created by the consumer and therefore are not closed
func (engine *Engine) Start(done chan bool) {
	defer engine.unloadAllSignatures()
	engine.signaturesMutex.RLock()
	for s, c := range engine.signatures {
		go signatureStart(s, c, engine.waitGroup)
	}
	engine.signaturesMutex.RUnlock()
	engine.consumeSources(done)
}

func (engine *Engine) unloadAllSignatures() {
	engine.signaturesMutex.Lock()
	defer engine.signaturesMutex.Unlock()
	for sig, c := range engine.signatures {
		sig.Close()
		close(c)
		delete(engine.signatures, sig)
	}
	engine.signaturesIndex = make(map[types.SignatureEventSelector][]types.Signature)
}

// matchHandler is a function that runs when a signature is matched
func (engine *Engine) matchHandler(res types.Finding) {
	engine.output <- res
}

// checkCompletion is a function that runs at the end of each input source
// closing tracee-rules if no more pending input sources exists
func (engine *Engine) completed() bool {
	if engine.inputs.Tracee == nil {
		// Unload signatures and wait for detection completion
		engine.unloadAllSignatures()
		engine.waitGroup.Wait()
		// Wait for the output to finish
		engine.waitGroup.Add(1)
		close(engine.output)
		engine.waitGroup.Wait()
		return true
	}
	return false
}

// consumeSources starts consuming the input sources
// it runs continuously until stopped by the done channel
func (engine *Engine) consumeSources(done <-chan bool) {
	for {
		select {
		case event, ok := <-engine.inputs.Tracee:
			if !ok {
				engine.signaturesMutex.RLock()
				for sig := range engine.signatures {
					se, err := sig.GetSelectedEvents()
					if err != nil {
						engine.logger.Printf("error getting selected events: %v", err)
						continue
					}
					for _, sel := range se {
						if sel.Source == "tracee" {
							_ = sig.OnSignal(types.SignalSourceComplete("tracee"))
							break
						}
					}
				}
				engine.signaturesMutex.RUnlock()
				engine.inputs.Tracee = nil
				if engine.completed() {
					return
				}
			} else if event != nil {
				engine.signaturesMutex.RLock()
				traceeEvt, ok := event.(tracee.Event)
				if !ok {
					engine.logger.Printf("invalid event received (should be of type tracee.Event)")
					engine.signaturesMutex.RUnlock()
					continue
				}

				pe, err := ToParsedEvent(traceeEvt)
				if err != nil {
					engine.logger.Printf("error converting tracee event to OPA ast.Value: %v", err)
					engine.signaturesMutex.RUnlock()
					continue
				}

				eventOrigin := analyzeEventOrigin(traceeEvt)
				for _, s := range engine.signaturesIndex[types.SignatureEventSelector{Source: "tracee", Name: traceeEvt.EventName, Origin: eventOrigin}] {
					engine.dispatchEvent(s, pe)
				}
				for _, s := range engine.signaturesIndex[types.SignatureEventSelector{Source: "tracee", Name: traceeEvt.EventName, Origin: ALL_EVENT_ORIGINS}] {
					engine.dispatchEvent(s, pe)
				}
				for _, s := range engine.signaturesIndex[types.SignatureEventSelector{Source: "tracee", Name: ALL_EVENT_TYPES, Origin: eventOrigin}] {
					engine.dispatchEvent(s, pe)
				}
				for _, s := range engine.signaturesIndex[types.SignatureEventSelector{Source: "tracee", Name: ALL_EVENT_TYPES, Origin: ALL_EVENT_ORIGINS}] {
					engine.dispatchEvent(s, pe)
				}
				engine.signaturesMutex.RUnlock()
			}
		case <-done:
			return
		}
	}
}

func (engine *Engine) dispatchEvent(s types.Signature, pe ParsedEvent) {
	switch {
	case strings.Contains(reflect.TypeOf(s).String(), "rego"):
		engine.signatures[s] <- pe
	default:
		engine.signatures[s] <- pe.Event
	}
}

//LoadSignature will store in Engine data structures the given signature and activate its handling business logics.
// It will return the signature ID as well as error.
func (engine *Engine) LoadSignature(signature types.Signature) (string, error) {
	selectedEvents, err := signature.GetSelectedEvents()
	if err != nil {
		return "", fmt.Errorf("failed to store signature: %w", err)
	}
	metadata, _ := signature.GetMetadata()
	// insert in engine.signatures map
	engine.signaturesMutex.Lock()
	defer engine.signaturesMutex.Unlock()
	if engine.signatures[signature] != nil {
		// signature already exists
		return metadata.ID, nil
	}
	c := make(chan types.Event)
	engine.signatures[signature] = c

	// insert in engine.signaturesIndex map
	for _, selectedEvent := range selectedEvents {
		if selectedEvent.Name == "" {
			selectedEvent.Name = ALL_EVENT_TYPES
		}
		if selectedEvent.Origin == "" {
			selectedEvent.Origin = ALL_EVENT_ORIGINS
		}
		if selectedEvent.Source == "" {
			engine.logger.Printf("signature %s doesn't declare an input source", metadata.Name)
		} else {
			engine.signaturesIndex[selectedEvent] = append(engine.signaturesIndex[selectedEvent], signature)
		}
	}
	if err := signature.Init(engine.matchHandler); err != nil {
		engine.logger.Printf("error initializing signature %s: %v", metadata.Name, err)

	}
	go signatureStart(signature, c, engine.waitGroup)
	return metadata.ID, nil
}

//UnloadSignature will remove from Engine data structures the given signature and stop its handling goroutine
func (engine *Engine) UnloadSignature(signatureId string) error {
	var signature types.Signature
	engine.signaturesMutex.RLock()
	for sig, _ := range engine.signatures {
		metadata, _ := sig.GetMetadata()
		if metadata.ID == signatureId {
			signature = sig
			break
		}
	}
	engine.signaturesMutex.RUnlock()
	if signature == nil {
		return fmt.Errorf("could not find signature with ID: %v", signatureId)
	}
	selectedEvents, err := signature.GetSelectedEvents()
	if err != nil {
		return fmt.Errorf("failed to unload signature: %w", err)
	}
	engine.signaturesMutex.Lock()
	defer engine.signaturesMutex.Unlock()
	// remove from engine.signatures map
	c, ok := engine.signatures[signature]
	if ok {
		delete(engine.signatures, signature)
		defer signature.Close()
		defer close(c)
	}
	// remove from engine.signaturesIndex map
	for _, selectedEvent := range selectedEvents {
		signatures := engine.signaturesIndex[selectedEvent]
		for i, sig := range signatures {
			metadata, _ := sig.GetMetadata()
			if metadata.ID == signatureId {
				// signature found, remove it
				signatures = append(signatures[:i], signatures[i+1:]...)
				engine.signaturesIndex[selectedEvent] = signatures
				break
			}
		}
	}
	return nil
}

// ParsedEvent holds the original tracee.Event and its OPA ast.Value representation.
type ParsedEvent struct {
	Event tracee.Event
	Value ast.Value
}

// ToParsedEvent enhances tracee.Event with OPA ast.Value. This is mainly used
// for performance optimization to avoid parsing tracee.Event multiple times.
func ToParsedEvent(e tracee.Event) (ParsedEvent, error) {
	u, err := e.ToUnstructured()
	if err != nil {
		return ParsedEvent{}, fmt.Errorf("unstructuring event: %w", err)
	}
	// TODO In OPA >= v0.30.0 we can try passing tracee.Event directly to get rid of ToUnstructured call.
	v, err := ast.InterfaceToValue(u)
	if err != nil {
		return ParsedEvent{}, fmt.Errorf("converting unstructured event to OPA ast.Value: %w", err)
	}
	return ParsedEvent{
		Event: e,
		Value: v,
	}, nil
}

func analyzeEventOrigin(event tracee.Event) string {
	if event.ContainerID != "" || event.ProcessID != event.HostProcessID {
		return EVENT_CONTAINER_ORIGIN
	} else {
		return EVENT_HOST_ORIGIN
	}
}
