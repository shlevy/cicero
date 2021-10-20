package cicero

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"encoding/json"

	"cirello.io/oversight"
	"github.com/liftbridge-io/go-liftbridge"
	"github.com/pkg/errors"
	"github.com/vivek-ng/concurrency-limiter/priority"
	nomad "github.com/hashicorp/nomad/api"
)

const invokeStreamName = "workflow.*.*.invoke"

type InvokerCmd struct {
	logger  *log.Logger
	tree    *oversight.Tree
	limiter *priority.PriorityLimiter
}

func (cmd *InvokerCmd) init() {
	if cmd.logger == nil {
		cmd.logger = log.New(os.Stderr, "invoker: ", log.LstdFlags)
	}

	if cmd.tree == nil {
		cmd.tree = oversight.New(oversight.WithSpecification(
			10,                    // number of restarts
			10*time.Minute,        // within this time period
			oversight.OneForOne(), // restart every task on its own
		))
	}

	if cmd.limiter == nil {
		// Increase priority of waiting goroutines every second.
		cmd.limiter = priority.NewLimiter(1, priority.WithDynamicPriority(1000))
	}
}

func (cmd *InvokerCmd) Run() error {
	cmd.init()
	cmd.tree.Add(cmd.listenToInvoke)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cmd.tree.Start(ctx); err != nil {
		return err
	}

	for {
		time.Sleep(1 * time.Hour)
	}
}

func (cmd *InvokerCmd) start(ctx context.Context) error {
	if err := cmd.listenToInvoke(ctx); err != nil {
		return err
	}

	<-ctx.Done()
	cmd.logger.Println("context was cancelled")
	return nil
}

func (cmd *InvokerCmd) listenToInvoke(ctx context.Context) error {
	cmd.init()
	cmd.logger.Println("Starting Invoker.listenToInvoke")

	client, err := connect(cmd.logger, []string{invokeStreamName})
	if err != nil {
		return err
	}
	defer client.Close()

	cmd.logger.Printf("Subscribing to %s\n", invokeStreamName)
	err = client.Subscribe(
		ctx,
		invokeStreamName,
		cmd.invokerSubscriber(ctx),
		liftbridge.StartAtLatestReceived(),
		liftbridge.Partition(0))

	if err != nil {
		return errors.WithMessage(err, "failed to subscribe")
	}

	return nil
}

func (cmd *InvokerCmd) invokerSubscriber(ctx context.Context) func(*liftbridge.Message, error) {
	return func(msg *liftbridge.Message, err error) {
		if err != nil {
			cmd.logger.Fatalf("error in liftbridge message: %s", err.Error())
		}

		inputs := string(msg.Value())
		cmd.logger.Println(msg.Timestamp(), msg.Offset(), string(msg.Key()), inputs)

		parts := strings.Split(msg.Subject(), ".")
		workflowName := parts[1]
		id, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil {
			cmd.logger.Printf("Invalid Workflow ID received, ignoring: %s\n", msg.Subject())
			return
		}

		err = cmd.invokeWorkflow(ctx, workflowName, id, inputs)
		if err != nil {
			cmd.logger.Println("Failed to invoke workflow", err)
		}
	}
}

func (cmd *InvokerCmd) invokeWorkflow(ctx context.Context, workflowName string, workflowId uint64, inputs string) error {
	workflow, err := nixInstantiateWorkflow(cmd.logger, workflowName, workflowId, inputs)
	if err != nil {
		return errors.WithMessage(err, "Invalid Workflow Definition, ignoring")
	}

	for stepName, step := range workflow.Steps {
		cmd.logger.Printf("Checking runnability of %s: %v\n", stepName, step.Job)
		if step.Job == nil {
			continue
		}

		err = cmd.invokeWorkflowStep(ctx, workflowName, workflowId, inputs, stepName, step)
		if err != nil {
			return err
		}
	}

	return nil
}

func (cmd *InvokerCmd) invokeWorkflowStep(ctx context.Context, workflowName string, workflowId uint64, inputs, stepName string, step workflowStep) error {
	cmd.limiter.Wait(context.Background(), priority.High)
	defer cmd.limiter.Finish()

	var err error

	switch *step.Type {
		case "nomad":
			var job nomad.Job
			err = json.Unmarshal([]byte(*step.Job), &struct{ Job *nomad.Job }{ Job: &job })
			if err != nil {
				return errors.WithMessage(err, "Invalid Nomad JSON Job")
			}

			var response *nomad.JobRegisterResponse
			response, _, err = nomadClient.Jobs().Register(&job, &nomad.WriteOptions{})
			if err == nil && len(response.Warnings) > 0 {
				cmd.logger.Println(response.Warnings)
			}
		default:
			cmd.logger.Printf("building %s.%s\n", workflowName, stepName)
			var output []byte
			output, err = nixBuild(ctx, workflowName, workflowId, stepName, inputs)
			if err != nil {
				cmd.logger.Println(string(output))
			}
	}

	cert := step.Failure
	if err == nil {
		cert = step.Success
	}

	publish(
		cmd.logger,
		fmt.Sprintf("workflow.%s.%d.cert", workflowName, workflowId),
		"workflow.*.*.cert",
		cert,
	)

	if err != nil {
		return errors.WithMessage(err, "Failed to run step")
	}

	return nil
}
