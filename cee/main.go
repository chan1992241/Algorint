package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-units"
	"github.com/pbnjay/memory"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

var supported_languages []string = strings.Split(os.Getenv("SUPPORTED_LANGUAGES"), ",")
var submission_queue string = strings.TrimSpace(os.Getenv("SUBMISSION_QUEUE"))
var rabbit_password string = strings.TrimSpace(os.Getenv("RABBITMQ_PASSWORD"))
var rabbitmq_username string = strings.TrimSpace(os.Getenv("RABBMITMQ_USERNAME"))

// var redis_host string = os.Getenv("REDIS_HOST")
var redis_password string = strings.TrimSpace(os.Getenv("REDIS_PASSWORD"))
var redis_sentinel_address string = strings.TrimSpace(os.Getenv("REDIS_SENTINELS"))

// TODO: remember to change this to the correct queue name
var queue_name string = strings.TrimSpace(os.Getenv("CEE_INTERPRETER_QUEUE_NAME"))

// language_details[language_name]["image"/"cmd"]
var languages_details map[string]map[string]string = map[string]map[string]string{}

func Initiate_Redis_Client() (*redis.Client, error) {
	context := context.Background()
	var redisClient *redis.Client
	if os.Getenv("ENVIRONMENT") == "production" {
		redis_host := os.Getenv("REDIS_HOST")
		redisClient = redis.NewClient(&redis.Options{
			Addr:     redis_host + ":6379",
			Password: redis_password,
			DB:       0,
		})
	} else {
		redisClient = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    "mymaster",
			SentinelAddrs: []string{redis_sentinel_address + ":5000"},
			Password:      redis_password,
		})
	}
	_, err := redisClient.Ping(context).Result()
	if err != nil {
		return nil, err
	}
	return redisClient, nil
}

func main() {
	cli, err := Initialize_Docker_Client()
	log.Printf("%v", supported_languages)
	if err != nil {
		log.Fatal("Failed to initialize docker client\n" + err.Error())
	}
	_, err = Initialize_Language_Executor(cli)
	if err != nil {
		log.Fatal("Failed to initialize language executor\n" + err.Error())
		return // Exiting function if there's an error
	}
	redis_client, err := Initiate_Redis_Client()
	if err != nil {
		log.Fatal("Failed to initialize redis client\n" + err.Error())
	}
	consume(cli, redis_client)
}

func Initialize_Language_Executor(cli *client.Client) (bool, error) {
	var wg sync.WaitGroup
	errorChan := make(chan error, len(supported_languages))
	finishChan := make(chan bool, len(supported_languages))
	for _, language := range supported_languages {
		wg.Add(1)
		go func(language string) {
			defer wg.Done()
			language_details := strings.Split(language, "@")
			log.Printf("Lanugage details: %s\n", language_details)
			language_name := language_details[0]
			language_image := language_details[1]
			language_extension := language_details[2]
			lanugage_type := language_details[3]
			if lanugage_type == "compiler" {
				languages_details[language_name] = map[string]string{
					"image":     language_image,
					"compile":   language_details[4],
					"execute":   language_details[5],
					"extension": language_extension,
					"type":      "compiler",
				}
			} else {
				languages_details[language_name] = map[string]string{
					"image":     language_image,
					"execute":   language_details[4],
					"extension": language_extension,
					"type":      "interpreter",
				}
			}
			out, err := cli.ImagePull(context.Background(), language_image, types.ImagePullOptions{})
			if err != nil {
				errorChan <- fmt.Errorf("failed to pull image %v: %v", language_image, err.Error())
				return
			}
			defer out.Close()
			io.Copy(io.Discard, out)

			log.Println("Finished pulling image:", language_details)
			finishChan <- true // Indicate that image has been pulled successfully
		}(language)
	}
	go func() {
		wg.Wait()
		close(errorChan)
		close(finishChan)
	}()
	var errorEncountered error
	finishedTasks := 0
	for {
		select {
		case err, ok := <-errorChan:
			if ok && err != nil && errorEncountered == nil { // Only recording the first error
				errorEncountered = err
			}
		case _, ok := <-finishChan:
			if ok {
				finishedTasks++
			}
		}

		if finishedTasks == len(supported_languages) || errorEncountered != nil {
			break
		}
	}

	if errorEncountered != nil {
		return false, errorEncountered
	}

	return true, nil
}

func Initialize_Docker_Client() (*client.Client, error) {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Printf("Failed to initialize docker client\n" + err.Error())
		return nil, err
	}
	_, err = cli.Ping(ctx)
	if err != nil {
		log.Printf("Failed to ping docker client\n" + err.Error())
		return nil, err
	}
	return cli, nil
}

func injectUsernamePasswordToRabbitMQURL(rabbitMQURL string, rabbitMQUsername string, rabbitMQPassword string) string {
	username := strings.TrimSpace(rabbitMQUsername)
	password := strings.TrimSpace(rabbitMQPassword)
	rabbitMQURL = strings.Replace(rabbitMQURL, "amqps://", fmt.Sprintf("amqps://%s:%s@", username, password), 1)
	return rabbitMQURL
}

func consume(cli *client.Client, redis_client *redis.Client) {
	conn, err := Initiate_MQ_Client()
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", err)
	}
	defer conn.Close()
	notify := conn.NotifyClose(make(chan *amqp.Error))
	ch, err := Initiate_MQ_Channel(conn)
	if err != nil {
		log.Fatal("Failed to open a channel", err)
	}
	defer ch.Close()
	err = Declare_MQ_Queue(ch, queue_name)
	if err != nil {
		log.Fatal("Failed to declare a queue", err)
	}
	if err := SetPrefetchCount(ch, 1); err != nil {
		log.Fatal("Failed to set prefetch count", err)
	}
	msgs, err := ch.Consume(
		queue_name,
		"cee",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatal("Failed to register a consumer", err)
	}
	forever := make(chan bool)
	go OnMessageReceived(msgs, cli, notify, redis_client)
	fmt.Println("Running...")
	<-forever
}

func OnMessageReceived(msgs <-chan amqp.Delivery, cli *client.Client, notifyClose <-chan *amqp.Error, redis_client *redis.Client) {
	go func() {
		for err := range notifyClose {
			log.Printf("AMQP connection closed: %v", err)
		}
	}()
	for d := range msgs {
		go Message_Handler(&d, cli, redis_client)
	}
}

func Message_Handler(d *amqp.Delivery, cli *client.Client, redis_client *redis.Client) {
	body := d.Body
	submission_id, err := Get_Submission_Token_From_MQ(body)
	startTime := time.Now()
	log.Printf("Handle a message: %s", submission_id)
	if err != nil {
		log.Printf("Error getting submission id from MQ\n" + err.Error())
	}
	result, err := Get_Submission_From_Redis(redis_client, submission_id)
	if err != nil {
		log.Printf("Failed to get submission from redis\n" + err.Error())
	}
	submission, err := Parse_Submission_From_Redis(result)
	if err != nil {
		log.Printf("Failed to parse submission from redis\n" + err.Error())
	}
	execution_channel := make(chan Execution_Result, len(submission.Stdin))
	threading_ctx, cancel := context.WithCancel(context.Background())
	log.Printf("Before running code for submission %s", submission_id)
	// Set Max number of goroutines
	guard := make(chan struct{}, 2)
	for index, code_input := range submission.Stdin {
		guard <- struct{}{} // would block if guard channel is already filled
		// base64 decode code_input\
		code_input_decoded, err := base64.StdEncoding.DecodeString(code_input)
		if err != nil {
			log.Printf("Failed to decode input\n" + err.Error())
		}
		code, err := base64.StdEncoding.DecodeString(submission.Code)
		if err != nil {
			log.Printf("Failed to decode code\n" + err.Error())
		}
		for _, replaces := range submission.Replace[index] {
			from, err := base64.StdEncoding.DecodeString(replaces.From)
			if err != nil {
				log.Printf("Failed to decode replace from\n" + err.Error())
			}
			to, err := base64.StdEncoding.DecodeString(replaces.To)
			if err != nil {
				log.Printf("Failed to decode replace to\n" + err.Error())
			}
			code = bytes.Replace(code, from, to, -1)
		}
		time_limit := submission.TimeLimit[index]
		mem_limit := submission.MemoryLimit[index]
		go func(index int) {
			RunCode(threading_ctx, cli, code, string(code_input_decoded), submission.Language, time_limit, mem_limit, index, execution_channel, submission_id)
			<-guard // read from the guard channel, allows another iteration to proceed
		}(index)
	}
	submission_results := []Execution_Result{}
	for i := 0; i < len(submission.Stdin); i++ {
		submission_result := <-execution_channel
		submission_results = append(submission_results, submission_result)
	}
	cancel()
	log.Printf("Done running code for submission %s", submission.SubmissionID)
	reordered_submission_results := make([]Execution_Result, len(submission_results))
	for _, result := range submission_results {
		reordered_submission_results[result.Submission_Index] = result
	}
	for _, result := range reordered_submission_results {
		base64_stdout := base64.StdEncoding.EncodeToString([]byte(result.Stdout))
		base64_stderr := base64.StdEncoding.EncodeToString([]byte(result.Stderr))
		submission.Stdout = append(submission.Stdout, base64_stdout)
		submission.Stderr = append(submission.Stderr, base64_stderr)
	}
	if err = Save_Submission_To_Redis(submission, redis_client); err != nil {
		log.Printf("Failed to save submission to redis\n" + err.Error())
	}
	log.Printf("Before Judge Submission for %s", submission.SubmissionID)
	statusCode, err := Judge_Submission(submission.SubmissionID)
	if err != nil {
		log.Printf("Failed to judge submission\n" + err.Error())
	}
	if statusCode == 200 {
		submission_after_judge, err := Get_Submission_From_Redis(redis_client, submission.SubmissionID)
		if err != nil {
			log.Printf("Failed to get submission after judge from redis\n" + err.Error())
		}
		submission_after_judge_parsed, err := Parse_Submission_From_Redis(submission_after_judge)
		if err != nil {
			log.Printf("Failed to parse submission after judge from redis\n" + err.Error())
		}
		submission_after_judge_parsed.Status = "done execution"
		if err = Save_Submission_To_Redis(submission_after_judge_parsed, redis_client); err != nil {
			log.Printf("Failed to save judged submission to redis\n" + err.Error())
		}
		log.Printf("After Judge Submission Response with updated status for %s", submission.SubmissionID)
	}
	elapsedTime := time.Since(startTime)
	d.Ack(false)
	log.Printf("Done processing submission: %s with %v", submission_id, elapsedTime.Seconds())
}

func Save_Submission_To_Redis(submission Submission, redis_client *redis.Client) error {
	ctx := context.Background()
	submission_json, err := json.Marshal(submission)
	if err != nil {
		return err
	}
	err = redis_client.Set(ctx, submission.SubmissionID, submission_json, 10*time.Minute).Err()
	if err != nil {
		return err
	}
	return nil
}

func Get_Submission_Token_From_MQ(body []byte) (string, error) {
	var submission_token Submission_Token
	err := json.Unmarshal(body, &submission_token)
	if err != nil {
		return "", err
	}
	return submission_token.Submission_Id, nil
}

func Get_Submission_From_Redis(redis_client *redis.Client, submission_token string) (string, error) {
	ctx := context.Background()
	val, err := redis_client.Get(ctx, submission_token).Result()
	if err != nil {
		return "", err
	}
	return val, nil
}

func Parse_Submission_From_Redis(submission string) (Submission, error) {
	var submission_struct Submission
	err := json.Unmarshal([]byte(submission), &submission_struct)
	if err != nil {
		return Submission{}, err
	}
	return submission_struct, nil
}

func Initiate_Docker_Client() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return cli, nil
}

func Initiate_MQ_Client() (*amqp.Connection, error) {
	var rabbitmq_url string = injectUsernamePasswordToRabbitMQURL(submission_queue, rabbitmq_username, rabbit_password)
	log.Printf("RabbitMQ URL: %s", rabbitmq_url)
	conn, err := amqp.Dial(rabbitmq_url)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func Declare_MQ_Queue(ch *amqp.Channel, queue_name string) error {
	_, err := ch.QueueDeclare(
		queue_name,
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return err
	}
	return nil
}

func Initiate_MQ_Channel(conn *amqp.Connection) (*amqp.Channel, error) {
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	return ch, nil
}

func Set_Data_To_Redis(redis_client *redis.Client, key string, value string) error {
	ctx := context.Background()
	err := redis_client.Set(ctx, key, value, 0).Err()
	if err != nil {
		return err
	}
	return nil
}

func Get_Data_From_Redis(redis_client *redis.Client, key string) (string, error) {
	ctx := context.Background()
	val, err := redis_client.Get(ctx, key).Result()
	if err != nil {
		return "", err
	}
	return val, nil
}

func executeCode(cli *client.Client, code []byte, input string, language string, time_limit int, mem_limit_mb int, submission_id string, submission_index int, execution_channel chan Execution_Result, threading_ctx context.Context) {
	ctx := context.Background()
	// Create a container
	log.Printf("Create a container for submission %v index %v", submission_id, submission_index)
	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      languages_details[language]["image"],
		Tty:        false,
		OpenStdin:  true,
		WorkingDir: "/app",
		Cmd:        strings.Split(languages_details[language]["execute"], " "),
	}, &container.HostConfig{
		AutoRemove: false,
		Resources: container.Resources{
			Ulimits: []*units.Ulimit{
				{
					Name: "nproc",
					Soft: 1024,
					Hard: 2048,
				},
			},
			Memory: int64(mem_limit_mb * 1024 * 1024),
		},
	}, nil, nil, "")
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	log.Printf("Add file to container for submission %v index %v", submission_id, submission_index)
	// Copy code to container
	content, err := Make_Archieve(fmt.Sprintf("code%s", languages_details[language]["extension"]), code)
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	err = cli.CopyToContainer(ctx, resp.ID, "/app", content, types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	log.Printf("Start container for submission %v index %v", submission_id, submission_index)
	// Start compiling container
	err = cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	log.Printf("Write input to container for submission %v index %v", submission_id, submission_index)
	// Write input to container
	hijackedResponse, err := cli.ContainerAttach(ctx, resp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: false,
		Stderr: false,
	})
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	_, err = hijackedResponse.Conn.Write([]byte(input + "\n"))
	if err != nil {
		log.Println("Error in writing input to container\n " + err.Error())
	}
	// Context with timeout
	time_limit_ctx, cancel := context.WithTimeout(context.Background(), time.Duration(time_limit)*time.Second)
	defer cancel()
	// Wait for container to finish
	statusCh, errCh := cli.ContainerWait(time_limit_ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Sandbox error, try to run again",
			})
		}
		cli.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{
			Force: true,
		})
	case <-time_limit_ctx.Done():
		cli.ContainerStop(ctx, resp.ID, container.StopOptions{})
		Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
			Submission_Index: submission_index,
			Stdout:           "",
			Stderr:           "Time Limit Exceeded",
		})
		cli.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{
			Force: true,
		})
	case statusCode := <-statusCh:
		if statusCode.StatusCode == 137 {
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Memory Limit Exceeded",
			})
		}
		log.Printf("Get container logs for submission %v index %v", submission_id, submission_index)
		out, err := cli.ContainerLogs(ctx, resp.ID, types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
		if err != nil {
			log.Println("Error in getting container logs\n " + err.Error())
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Something wrong " + err.Error(),
			})
			return
		}
		stdoutput := new(bytes.Buffer)
		stderror := new(bytes.Buffer)
		_, err = stdcopy.StdCopy(stdoutput, stderror, out)
		if err != nil {
			log.Println("Error in copying container logs\n " + err.Error())
		}
		cli.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{
			Force: true,
		})
		Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
			Submission_Index: submission_index,
			Stdout:           stdoutput.String(),
			Stderr:           stderror.String(),
		})
		defer out.Close()
	case <-threading_ctx.Done():
		cli.ContainerRemove(ctx, resp.ID, types.ContainerRemoveOptions{
			Force: true,
		})
		return
	}
}

func compileCode(cli *client.Client, code []byte, input string, language string, time_limit int, mem_limit_mb int, submission_id string, submission_index int, execution_channel chan Execution_Result, threading_ctx context.Context) {
	ctx := context.Background()
	volumeName := fmt.Sprintf("submission-%s-%d", submission_id, submission_index)
	cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   volumeName,
		Driver: "local",
	})
	// Create compiling a container
	log.Printf("Create a container for submission %v index %v", submission_id, submission_index)
	compilingResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      languages_details[language]["image"],
		Tty:        false,
		OpenStdin:  false,
		WorkingDir: "/app",
		Cmd:        strings.Split(languages_details[language]["compile"], " "),
	}, &container.HostConfig{
		AutoRemove: false,
		Binds:      []string{fmt.Sprintf("%s:/app", volumeName)},
		Resources: container.Resources{
			Ulimits: []*units.Ulimit{
				{
					Name: "nproc",
					Soft: 1024,
					Hard: 2048,
				},
			},
			Memory: int64(mem_limit_mb * 1024 * 1024),
		},
	}, nil, nil, "")
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	log.Printf("Add file to compile container for submission %v index %v", submission_id, submission_index)
	// Copy code to container
	content, err := Make_Archieve(fmt.Sprintf("code%s", languages_details[language]["extension"]), code)
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	err = cli.CopyToContainer(ctx, compilingResp.ID, "/app", content, types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
	})
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	// Start compiling container
	err = cli.ContainerStart(ctx, compilingResp.ID, types.ContainerStartOptions{})
	if err != nil {
		log.Println("Error in starting compiling container\n " + err.Error())
	}
	// Wait for container to running
	compile_time_ctx, cancel := context.WithTimeout(context.Background(), time.Duration(5)*time.Second)
	compilingStatusCh, compilingErrCh := cli.ContainerWait(compile_time_ctx, compilingResp.ID, container.WaitConditionNotRunning)
	// Context with timeout
	defer cancel()
	select {
	case err := <-compilingErrCh:
		if err != nil {
			cli.ContainerRemove(ctx, compilingResp.ID, types.ContainerRemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
			log.Printf("Error in compiling container\n " + err.Error())
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Time Limit Exceeded",
			})
		}
	case <-compile_time_ctx.Done():
		cli.ContainerRemove(ctx, compilingResp.ID, types.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
			Submission_Index: submission_index,
			Stdout:           "",
			Stderr:           "Compile Time Limit Exceeded",
		})
	case statusCode := <-compilingStatusCh:
		if statusCode.StatusCode == 137 {
			cli.ContainerRemove(ctx, compilingResp.ID, types.ContainerRemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Compile Memory Limit Exceeded",
			})
		}
		if statusCode.StatusCode == 127 {
			cli.ContainerRemove(ctx, compilingResp.ID, types.ContainerRemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Compile Error, Invalid Command",
			})
		}
		if statusCode.StatusCode == 1 {
			log.Printf("Status code: %v", statusCode.StatusCode)
			log.Printf("Get container logs for submission %v index %v", submission_id, submission_index)
			out, err := cli.ContainerLogs(ctx, compilingResp.ID, types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
			if err != nil {
				log.Println("Error in getting container logs\n " + err.Error())
				Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
					Submission_Index: submission_index,
					Stdout:           "",
					Stderr:           "Error in compiling container " + err.Error(),
				})
				return
			}
			stdoutput := new(bytes.Buffer)
			stderror := new(bytes.Buffer)
			_, err = stdcopy.StdCopy(stdoutput, stderror, out)
			if err != nil {
				log.Println("Error in copying container logs\n " + err.Error())
			}
			cli.ContainerRemove(ctx, compilingResp.ID, types.ContainerRemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           stdoutput.String(),
				Stderr:           stderror.String(),
			})
			defer out.Close()
			return
		}
	case <-threading_ctx.Done():
		return
	}
	// Executing code
	executeResp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      languages_details[language]["image"],
		Tty:        false,
		OpenStdin:  true,
		WorkingDir: "/app",
		Cmd:        strings.Split(languages_details[language]["execute"], " "),
	}, &container.HostConfig{
		AutoRemove: false,
		Binds:      []string{fmt.Sprintf("%s:/app", volumeName)},
		Resources: container.Resources{
			Ulimits: []*units.Ulimit{
				{
					Name: "nproc",
					Soft: 100,
					Hard: 1024,
				},
			},
			Memory: int64(mem_limit_mb * 1024 * 1024),
		},
	}, nil, nil, "")
	if err != nil {
		log.Println("Error in creating executing container\n " + err.Error())
	}
	log.Printf("Start executing container for submission %v index %v", submission_id, submission_index)
	err = cli.ContainerStart(ctx, executeResp.ID, types.ContainerStartOptions{})
	if err != nil {
		log.Println("Error in creating executing container\n " + err.Error())
	}
	// Write input to container
	log.Printf("Attach executing container for submission %v index %v", submission_id, submission_index)
	hijackedResponse, err := cli.ContainerAttach(ctx, executeResp.ID, types.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: false,
		Stderr: false,
	})
	if err != nil {
		log.Println("Error in creating container\n " + err.Error())
	}
	log.Printf("Attach executing container for submission %v index %v", submission_id, submission_index)
	_, err = hijackedResponse.Conn.Write([]byte(input + "\n"))
	if err != nil {
		log.Println("Error in writing input to container\n " + err.Error())
	}
	log.Printf("Write input to container for submission %v index %v", submission_id, submission_index)
	executing_time_limit_ctx, cancel_execute := context.WithTimeout(context.Background(), time.Duration(time_limit)*time.Second)
	executingStatusCh, executingErrCh := cli.ContainerWait(executing_time_limit_ctx, executeResp.ID, container.WaitConditionNotRunning)
	defer cancel_execute()
	select {
	case err := <-executingErrCh:
		if err != nil {
			cli.ContainerRemove(ctx, executeResp.ID, types.ContainerRemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Sandbox error, try to run again",
			})
		}
	case <-executing_time_limit_ctx.Done():
		cli.ContainerRemove(ctx, executeResp.ID, types.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
			Submission_Index: submission_index,
			Stdout:           "",
			Stderr:           "Run Time Limit Exceeded",
		})
	case statusCode := <-executingStatusCh:
		if statusCode.StatusCode == 137 {
			cli.ContainerRemove(ctx, executeResp.ID, types.ContainerRemoveOptions{
				Force:         true,
				RemoveVolumes: true,
			})
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           "Run Time Memory Limit Exceeded",
			})
		}
		log.Printf("Get container logs for submission %v index %v", submission_id, submission_index)
		out, err := cli.ContainerLogs(ctx, executeResp.ID, types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
		if err != nil {
			log.Println("Error in getting container logs\n " + err.Error())
			Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
				Submission_Index: submission_index,
				Stdout:           "",
				Stderr:           " " + err.Error(),
			})
			return
		}
		stdoutput := new(bytes.Buffer)
		stderror := new(bytes.Buffer)
		_, err = stdcopy.StdCopy(stdoutput, stderror, out)
		if err != nil {
			log.Println("Error in copying container logs\n " + err.Error())
		}
		cli.ContainerRemove(ctx, executeResp.ID, types.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		cli.ContainerRemove(ctx, compilingResp.ID, types.ContainerRemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		})
		cli.VolumeRemove(ctx, volumeName, true)
		Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
			Submission_Index: submission_index,
			Stdout:           stdoutput.String(),
			Stderr:           stderror.String(),
		})
		defer out.Close()
	case <-threading_ctx.Done():
		return
	}
}

func RunCode(threading_ctx context.Context, cli *client.Client, code []byte, input string, language string, time_limit int, mem_limit_mb int, submission_index int, execution_channel chan Execution_Result, submission_id string) {
	if languages_details[language]["type"] == "interpreter" {
		executeCode(cli, code, input, language, time_limit, mem_limit_mb, submission_id, submission_index, execution_channel, threading_ctx)
	} else if languages_details[language]["type"] == "compiler" {
		compileCode(cli, code, input, language, time_limit, mem_limit_mb, submission_id, submission_index, execution_channel, threading_ctx)
	} else {
		Put_Execution_Result_To_Channel(execution_channel, Execution_Result{
			Submission_Index: submission_index,
			Stdout:           "",
			Stderr:           "Unsupported language",
		})
	}
}

func Put_Execution_Result_To_Channel(execution_channel chan Execution_Result, execution_result Execution_Result) {
	execution_channel <- execution_result
}

func Make_Archieve(filename string, data []byte) (*bytes.Reader, error) {
	var buf bytes.Buffer
	tarWriter := tar.NewWriter(&buf)
	tarHeader := &tar.Header{
		Name: filename,
		Mode: 0777,
		Size: int64(len(data)),
	}
	err := tarWriter.WriteHeader(tarHeader)
	if err != nil {
		log.Printf("Error in writing tar header\n")
		return nil, err
	}
	_, err = tarWriter.Write(data)
	if err != nil {
		log.Printf("Error in writing tar data\n")
		return nil, err
	}
	err = tarWriter.Close()
	if err != nil {
		log.Printf("Error in closing tar writer\n")
		return nil, err
	}
	content := bytes.NewReader(buf.Bytes())
	return content, nil
}

func Judge_Submission(submission_id string) (int, error) {
	if submission_id == "" {
		return -1, fmt.Errorf("submission_id is empty")
	}
	jsonBody := []byte(fmt.Sprintf(`{"submission_id": "%s"}`, submission_id))
	bodyReader := bytes.NewReader(jsonBody)
	req, err := http.NewRequest("POST", "http://judge.judge.svc.cluster.local/judge", bodyReader)
	if err != nil {
		req.Body.Close()
		return -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func SetPrefetchCount(channel *amqp.Channel, prefetch_count int) error {
	err := channel.Qos(prefetch_count, 0, false)
	if err != nil {
		return err
	}
	return nil
}

func PrintMemUsage() {
	log.Printf("Total system memory: %d\n", memory.TotalMemory())
	log.Printf("Free memory: %d\n", memory.FreeMemory())
}
