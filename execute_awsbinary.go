// +build lambdabinary

package sparta

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	awsLambdaGo "github.com/aws/aws-lambda-go/lambda"
	awsLambdaContext "github.com/aws/aws-lambda-go/lambdacontext"
	cloudformationResources "github.com/mweagle/Sparta/aws/cloudformation/resources"
	gocf "github.com/mweagle/go-cloudformation"
	"github.com/sirupsen/logrus"
)

// StampedServiceName is the name stamp
// https://blog.cloudflare.com/setting-go-variables-at-compile-time/
// StampedServiceName is the serviceName stamped into this binary
var StampedServiceName string

// StampedBuildID is the buildID stamped into the binary
var StampedBuildID string

var discoveryInfo *DiscoveryInfo
var once sync.Once

func initDiscoveryInfo() {
	info, _ := Discover()
	discoveryInfo = info
}

func awsLambdaFunctionName(internalFunctionName string) gocf.Stringable {
	// TODO - move this to use SSM so that it's not human editable?
	// But discover information is per-function, not per stack.
	// Could we put the stack discovery info in there?
	once.Do(initDiscoveryInfo)
	sanitizedName := awsLambdaInternalName(internalFunctionName)

	return gocf.String(fmt.Sprintf("%s%s%s",
		discoveryInfo.StackName,
		functionNameDelimiter,
		sanitizedName))
}

func takesContext(handler reflect.Type) bool {
	handlerTakesContext := false
	if handler.NumIn() > 0 {
		contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
		argumentType := handler.In(0)
		handlerTakesContext = argumentType.Implements(contextType)
	}
	return handlerTakesContext
}

func tappedHandler(handlerSymbol interface{},
	logger *logrus.Logger) interface{} {

	// Tap the call chain to inject the context params...
	handler := reflect.ValueOf(handlerSymbol)
	handlerType := reflect.TypeOf(handlerSymbol)
	takesContext := takesContext(handlerType)

	return func(ctx context.Context, msg json.RawMessage) (interface{}, error) {
		ctx = context.WithValue(ctx, ContextKeyLogger, logger)

		// Create the entry logger that has some context information
		var logrusEntry *logrus.Entry
		lambdaContext, lambdaContextOk := awsLambdaContext.FromContext(ctx)
		if lambdaContextOk {
			logrusEntry = logrus.
				NewEntry(logger).
				WithFields(logrus.Fields{
					"reqID": lambdaContext.AwsRequestID,
					"arn":   lambdaContext.InvokedFunctionArn,
					"build": StampedBuildID,
				})
		} else {
			logrusEntry = logrus.
				NewEntry(logger).
				WithFields(logrus.Fields{})
		}
		ctx = context.WithValue(ctx, ContextKeyRequestLogger, logrusEntry)

		// construct arguments
		var args []reflect.Value
		if takesContext {
			args = append(args, reflect.ValueOf(ctx))
		}
		if (handlerType.NumIn() == 1 && !takesContext) ||
			handlerType.NumIn() == 2 {
			eventType := handlerType.In(handlerType.NumIn() - 1)
			event := reflect.New(eventType)
			unmarshalErr := json.Unmarshal(msg, event.Interface())
			if unmarshalErr != nil {
				return nil, unmarshalErr
			}
			args = append(args, event.Elem())
		}
		response := handler.Call(args)

		// If the user function
		// convert return values into (interface{}, error)
		var err error
		if len(response) > 0 {
			if errVal, ok := response[len(response)-1].Interface().(error); ok {
				err = errVal
			}
		}
		var val interface{}
		if len(response) > 1 {
			val = response[0].Interface()
		}
		return val, err
	}
}

// Execute creates an HTTP listener to dispatch execution. Typically
// called via Main() via command line arguments.
func Execute(serviceName string,
	lambdaAWSInfos []*LambdaAWSInfo,
	logger *logrus.Logger) error {

	// Initialize the discovery service
	initializeDiscovery(logger)

	// Find the function name based on the dispatch
	// https://docs.aws.amazon.com/lambda/latest/dg/current-supported-versions.html
	requestedLambdaFunctionName := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	// Log any info when we start up...
	//platformLogSysInfo(requestedLambdaFunctionName, logger)

	/*
		There are three types of targets:
			- User functions
			- User custom resources
			- Sparta custom resources
	*/
	// Based on the environment variable, setup the proper listener...
	var lambdaFunctionName gocf.Stringable
	testAWSName := ""
	var handlerSymbol interface{}
	knownNames := []string{}
	for _, eachLambdaInfo := range lambdaAWSInfos {
		lambdaFunctionName = awsLambdaFunctionName(eachLambdaInfo.lambdaFunctionName())
		testAWSName = lambdaFunctionName.String().Literal

		knownNames = append(knownNames, testAWSName)
		if requestedLambdaFunctionName == testAWSName {
			handlerSymbol = eachLambdaInfo.handlerSymbol
		}
		// User defined custom resource handler?
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			lambdaFunctionName = awsLambdaFunctionName(eachCustomResource.userFunctionName)
			testAWSName = lambdaFunctionName.String().Literal
			knownNames = append(knownNames, testAWSName)
			if requestedLambdaFunctionName == testAWSName {
				handlerSymbol = eachCustomResource.handlerSymbol
			}
		}
		if handlerSymbol != nil {
			break
		}
	}
	if handlerSymbol == nil {
		// So the sanitized name is what we use to dispatch. For
		// that we need a reverse lookup?
		customResourceTypes := []string{
			cloudformationResources.HelloWorld,
			cloudformationResources.S3LambdaEventSource,
			cloudformationResources.SNSLambdaEventSource,
			cloudformationResources.SESLambdaEventSource,
			cloudformationResources.CloudWatchLogsLambdaEventSource,
			cloudformationResources.ZipToS3Bucket,
		}
		customResourceType := ""
		for _, eachCustomResourceType := range customResourceTypes {
			lambdaFunctionName = awsLambdaFunctionName(eachCustomResourceType)
			testAWSName = lambdaFunctionName.String().Literal

			logger.WithFields(logrus.Fields{
				"customResourceType": eachCustomResourceType,
				"computedLambdaName": testAWSName,
			}).Debug("Checking custom resource")

			knownNames = append(knownNames, testAWSName)

			if requestedLambdaFunctionName == testAWSName {
				customResourceType = eachCustomResourceType
				break
			}
		}
		if customResourceType != "" {
			customHandler := cloudformationResources.NewCustomResourceLambdaHandler(customResourceType,
				logger)
			if customHandler != nil {
				handlerSymbol = customHandler
			}
		}
	}

	if handlerSymbol == nil {
		errorMessage := fmt.Errorf("No handler found for lambdaName: %s. Known: %s",
			requestedLambdaFunctionName,
			strings.Join(knownNames, ","))
		logger.Error(errorMessage)
		return errorMessage
	}

	// Startup our version...
	tappedHandler := tappedHandler(handlerSymbol, logger)
	awsLambdaGo.Start(tappedHandler)
	return nil
}