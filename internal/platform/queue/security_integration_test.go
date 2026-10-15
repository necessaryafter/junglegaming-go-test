//go:build integration

package queue_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/google/uuid"
)

func TestRealProducerPermissions(t *testing.T) {
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT must point to MiniStack with AUTH=true")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}

	admin := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
	identities := iam.NewFromConfig(cfg, func(o *iam.Options) { o.BaseEndpoint = aws.String(endpoint) })
	id, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}

	prefix := "security-" + id.String()
	urls := make(map[string]string)
	arns := make(map[string]string)

	for _, role := range []string{"REQUESTS", "EVENTS", "REQUESTS_DLQ", "EVENTS_DLQ"} {
		out, err := admin.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName: aws.String(prefix + "-" + role + ".fifo"), Attributes: map[string]string{"FifoQueue": "true"},
		})
		if err != nil {
			t.Fatal(err)
		}

		urls[role] = aws.ToString(out.QueueUrl)
		url := urls[role]
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, err := admin.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)}); err != nil {
				t.Errorf("delete security queue: %v", err)
			}
		})

		attributes, err := admin.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: out.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			t.Fatal(err)
		}

		arns[role+"_ARN"] = attributes.Attributes["QueueArn"]
	}

	policy := func(file string) string {
		t.Helper()

		data, err := os.ReadFile("../../../docker/sqs/" + file)
		if err != nil {
			t.Fatal(err)
		}

		document := string(data)

		for name, value := range arns {
			document = strings.ReplaceAll(document, "${"+name+"}", value)
		}

		return document
	}
	producerPolicy := policy("producer-policy.json")
	runtimePolicy := policy("runtime-policy.json")

	newUser := func(role string, document string) (*sqs.Client, aws.Credentials) {
		t.Helper()

		name := prefix + "-" + role
		if _, err := identities.CreateUser(ctx, &iam.CreateUserInput{UserName: aws.String(name)}); err != nil {
			t.Fatal(err)
		}

		if _, err := identities.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
			UserName: aws.String(name), PolicyName: aws.String("access"), PolicyDocument: aws.String(document),
		}); err != nil {
			t.Fatal(err)
		}

		key, err := identities.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: aws.String(name)})
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if _, err := identities.DeleteAccessKey(cleanup, &iam.DeleteAccessKeyInput{
				UserName: aws.String(name), AccessKeyId: key.AccessKey.AccessKeyId,
			}); err != nil {
				t.Errorf("delete security access key: %v", err)
			}

			if _, err := identities.DeleteUserPolicy(cleanup, &iam.DeleteUserPolicyInput{
				UserName: aws.String(name), PolicyName: aws.String("access"),
			}); err != nil {
				t.Errorf("delete security policy: %v", err)
			}

			if _, err := identities.DeleteUser(cleanup, &iam.DeleteUserInput{UserName: aws.String(name)}); err != nil {
				t.Errorf("delete security identity: %v", err)
			}
		})

		credential := aws.Credentials{
			AccessKeyID: aws.ToString(key.AccessKey.AccessKeyId), SecretAccessKey: aws.ToString(key.AccessKey.SecretAccessKey),
		}
		userCfg := cfg.Copy()
		userCfg.Credentials = credentials.NewStaticCredentialsProvider(credential.AccessKeyID, credential.SecretAccessKey, "")

		return sqs.NewFromConfig(userCfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) }), credential
	}
	a, aKey := newUser("a", producerPolicy)
	b, bKey := newUser("b", producerPolicy)
	runtime, _ := newUser("runtime", runtimePolicy)

	if aKey.AccessKeyID == bKey.AccessKeyID {
		t.Fatal("producers share an AWS identity")
	}

	send := func(client *sqs.Client, role string, deduplicationID string) error {
		_, err := client.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl: aws.String(urls[role]), MessageBody: aws.String("permission probe"),
			MessageGroupId: aws.String("security"), MessageDeduplicationId: aws.String(deduplicationID),
		})

		return err
	}

	if err := send(a, "REQUESTS", "a"); err != nil {
		t.Fatal(err)
	}

	if err := send(b, "REQUESTS", "b"); err != nil {
		t.Fatal(err)
	}

	if err := send(runtime, "EVENTS", "event"); err != nil {
		t.Fatal(err)
	}

	for name, operation := range map[string]func() error{
		"producer reads requests": func() error {
			_, err := a.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(urls["REQUESTS"])})
			return err
		},
		"producer publishes output events":       func() error { return send(a, "EVENTS", "forbidden") },
		"runtime impersonates an input producer": func() error { return send(runtime, "REQUESTS", "forbidden") },
		"producer purges requests": func() error {
			_, err := a.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: aws.String(urls["REQUESTS"])})
			return err
		},
		"producer changes queue policy": func() error {
			_, err := b.SetQueueAttributes(ctx, &sqs.SetQueueAttributesInput{
				QueueUrl: aws.String(urls["REQUESTS"]), Attributes: map[string]string{"VisibilityTimeout": "1"},
			})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertBrokerDenied(t, operation())
		})
	}

	// A forged secret must fail authentication even with a known producer key ID.
	badCfg := cfg.Copy()
	badCfg.Credentials = credentials.NewStaticCredentialsProvider(aKey.AccessKeyID, "wrong-secret", "")
	bad := sqs.NewFromConfig(badCfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
	assertBrokerDenied(t, send(bad, "REQUESTS", "forged-secret"))

	deny := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"sqs:*","Resource":"*"}]}`
	if _, err := identities.PutUserPolicy(ctx, &iam.PutUserPolicyInput{
		UserName: aws.String(prefix + "-a"), PolicyName: aws.String("access"), PolicyDocument: aws.String(deny),
	}); err != nil {
		t.Fatal(err)
	}

	assertBrokerDenied(t, send(a, "REQUESTS", "revoked-a"))

	if err := send(b, "REQUESTS", "still-authorized-b"); err != nil {
		t.Fatalf("revoking producer A affected producer B: %v", err)
	}

	// Runtime can receive and acknowledge input, but producers cannot.
	received, err := runtime.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(urls["REQUESTS"]), MaxNumberOfMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(received.Messages) != 1 {
		t.Fatal("runtime could not receive an authorized input")
	}

	receipt := received.Messages[0].ReceiptHandle
	_, err = a.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(urls["REQUESTS"]), ReceiptHandle: receipt})
	assertBrokerDenied(t, err)

	if _, err := runtime.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl: aws.String(urls["REQUESTS"]), ReceiptHandle: receipt,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertBrokerDenied(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("broker accepted a forbidden operation; IAM enforcement must be enabled")
	}

	var response *smithyhttp.ResponseError
	if !errors.As(err, &response) {
		t.Fatalf("expected a broker denial, got %v", err)
	}

	if response.HTTPStatusCode() != 403 {
		t.Fatalf("broker status = %d, want permission denial", response.HTTPStatusCode())
	}
}
