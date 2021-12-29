package component

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/georgysavva/scany/pgxscan"
	"github.com/google/uuid"
	nomad "github.com/hashicorp/nomad/api"
	"github.com/jackc/pgx/v4"
	"github.com/pkg/errors"

	"github.com/input-output-hk/cicero/src/application"
	"github.com/input-output-hk/cicero/src/application/service"
	"github.com/input-output-hk/cicero/src/config"
	"github.com/input-output-hk/cicero/src/domain"
)

type NomadEventConsumer struct {
	Logger              *log.Logger
	MessageQueueService service.MessageQueueService
	NomadEventService   service.NomadEventService
	RunService          service.RunService
	Db                  config.PgxIface
	NomadClient         application.NomadClient
}

func (self *NomadEventConsumer) Start(ctx context.Context) error {
	self.Logger.Println("Starting NomadEventConsumer")

	index, err := self.NomadEventService.GetLastNomadEvent()
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return errors.WithMessage(err, "Could not get last Nomad event index")
	}
	index += 1

	self.Logger.Println("Listening to Nomad events starting at index", index)

	stream, err := self.NomadClient.EventStream(ctx, index)
	if err != nil {
		return errors.WithMessage(err, "Could not listen to Nomad events")
	}

	for {
		events := <-stream
		if events.Err != nil {
			return errors.WithMessage(events.Err, "Error getting next events from Nomad event stream")
		}

		if events.Index < index {
			// We always get the last event even if we start at
			// an index greater than the last so we have to ignore it.
			// https://github.com/hashicorp/nomad/issues/11296
			continue
		}

		for _, event := range events.Events {
			if err := self.Db.BeginFunc(ctx, func(tx pgx.Tx) error {
				self.Logger.Println("Processing Nomad Event with index:", event.Index)
				return self.processNomadEvent(&event, tx)
			}); err != nil {
				return errors.WithMessagef(err, "Error processing Nomad event with index: %d", event.Index)
			}
		}

		index = events.Index
	}
}
func (self *NomadEventConsumer) processNomadEvent(event *nomad.Event, tx pgx.Tx) error {
	if err := self.handleNomadEvent(event, tx); err != nil {
		return errors.WithMessage(err, "Error handling Nomad event")
	}
	if err := self.NomadEventService.Save(tx, event); err != nil {
		return errors.WithMessage(err, "Error to save Nomad event")
	}
	return nil
}

func (self *NomadEventConsumer) handleNomadEvent(event *nomad.Event, tx pgx.Tx) error {
	if event.Topic == "Allocation" && event.Type == "AllocationUpdated" {
		allocation, err := event.Allocation()
		if err != nil {
			return errors.WithMessage(err, "Error getting Nomad event's allocation")
		}
		return self.handleNomadAllocationEvent(allocation, tx)
	}
	return nil
}

func (self *NomadEventConsumer) handleNomadAllocationEvent(allocation *nomad.Allocation, tx pgx.Tx) error {
	if !allocation.ClientTerminalStatus() {
		self.Logger.Printf("Ignoring allocation event with non-terminal client status %q", allocation.ClientStatus)
		return nil
	}

	id, err := uuid.Parse(allocation.JobID)
	if err != nil {
		return nil
	}

	run, err := self.RunService.GetByNomadJobId(id)
	if err != nil {
		if pgxscan.NotFound(err) {
			self.Logger.Printf("Ignoring Nomad event for Job with ID %q (no such Run)", id)
			return nil
		}
		return err
	}

	var fact *interface{}
	switch allocation.ClientStatus {
	case "complete":
		fact = &run.Success
	case "failed":
		fact = &run.Failure
	}
	if fact == nil {
		return nil
	}

	modifyTime := time.Unix(
		allocation.ModifyTime/int64(time.Second),
		allocation.ModifyTime%int64(time.Second),
	).UTC()
	run.FinishedAt = &modifyTime

	if err := self.RunService.Update(tx, &run); err != nil {
		return errors.WithMessagef(err, "Failed to update Run with ID %q", run.NomadJobID)
	}

	if message, err := json.Marshal(domain.Fact{
		RunId: &id,
		Value: *fact,
	}); err != nil {
		return errors.WithMessage(err, "Failed to marshal Fact")
	} else if err := self.MessageQueueService.Publish(
		domain.FactCreateStreamName.String(),
		domain.FactCreateStreamName,
		message,
	); err != nil {
		return errors.WithMessage(err, "Could not publish Fact")
	}

	if _, _, err := self.NomadClient.JobsDeregister(run.NomadJobID.String(), false, &nomad.WriteOptions{}); err != nil {
		return errors.WithMessagef(err, "Failed to deregister Nomad job with ID %q", run.NomadJobID)
	}

	return nil
}