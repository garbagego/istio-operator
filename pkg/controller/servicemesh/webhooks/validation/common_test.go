package validation

import (
	"context"
	"encoding/json"
	"fmt"

	admission "k8s.io/api/admission/v1beta1"
	authentication "k8s.io/api/authentication/v1"
	authorization "k8s.io/api/authorization/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	atypes "sigs.k8s.io/controller-runtime/pkg/webhook/admission/types"

	"github.com/maistra/istio-operator/pkg/controller/common"
)

var ctx = common.NewContextWithLog(context.Background(), logf.Log)

var userInfo = authentication.UserInfo{
	Username: "joe-user",
	UID:      "some-UID",
	Groups:   []string{"some-group"},
	Extra: map[string]authentication.ExtraValue{
		"key": []string{"extra-value"},
	},
}

func createSubjectAccessReviewReactor(allowClusterScope, allowNamespaceScope bool, errorToReturn error) func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
	return func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
		createAction := action.(clienttesting.CreateAction)
		sar := createAction.GetObject().(*authorization.SubjectAccessReview)
		if sar.Spec.ResourceAttributes.Namespace == "" {
			sar.Status.Allowed = allowClusterScope
		} else {
			sar.Status.Allowed = allowNamespaceScope
		}
		return true, sar.DeepCopy(), errorToReturn
	}
}

func createCreateRequest(obj interface{}) atypes.Request {
	request := atypes.Request{
		AdmissionRequest: &admission.AdmissionRequest{
			Operation: admission.Create,
			Object:    toRawExtension(obj),
			UserInfo:  userInfo,
		},
	}
	return request
}

func createUpdateRequest(oldObj, newObj interface{}) atypes.Request {
	request := atypes.Request{
		AdmissionRequest: &admission.AdmissionRequest{
			Operation: admission.Update,
			Object:    toRawExtension(newObj),
			OldObject: toRawExtension(oldObj),
			UserInfo:  userInfo,
		},
	}
	return request
}

func createDeleteRequest(obj interface{}) atypes.Request {
	request := atypes.Request{
		AdmissionRequest: &admission.AdmissionRequest{
			Operation: admission.Delete,
			Object:    toRawExtension(obj),
			UserInfo:  userInfo,
		},
	}
	return request
}

func toRawExtension(obj interface{}) runtime.RawExtension {
	memberJson, err := json.Marshal(obj)
	if err != nil {
		panic(fmt.Sprintf("Could not marshal object to JSON: %s", err))
	}

	return runtime.RawExtension{
		Raw: memberJson,
	}
}

func now() *meta.Time {
	now := meta.Now()
	return &now
}
