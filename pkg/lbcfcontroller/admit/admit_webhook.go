/*
 * Copyright 2019 THL A29 Limited, a Tencent company.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package admit

import (
	"encoding/json"
	"fmt"

	lbcfapi "git.tencent.com/tke/lb-controlling-framework/pkg/apis/lbcf.tke.cloud.tencent.com/v1beta1"
	lbcflister "git.tencent.com/tke/lb-controlling-framework/pkg/client-go/listers/lbcf.tke.cloud.tencent.com/v1beta1"
	"git.tencent.com/tke/lb-controlling-framework/pkg/lbcfcontroller/util"
	"git.tencent.com/tke/lb-controlling-framework/pkg/lbcfcontroller/webhooks"

	admission "k8s.io/api/admission/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog"
)

type AdmitWebhook interface {
	ValidateAdmitWebhook
	MutateAdmitWebhook
}

type ValidateAdmitWebhook interface {
	ValidateLoadBalancerCreate(*admission.AdmissionReview) *admission.AdmissionResponse
	ValidateLoadBalancerUpdate(*admission.AdmissionReview) *admission.AdmissionResponse
	ValidateLoadBalancerDelete(*admission.AdmissionReview) *admission.AdmissionResponse

	ValidateDriverCreate(*admission.AdmissionReview) *admission.AdmissionResponse
	ValidateDriverUpdate(*admission.AdmissionReview) *admission.AdmissionResponse
	ValidateDriverDelete(*admission.AdmissionReview) *admission.AdmissionResponse

	ValidateBackendGroupCreate(*admission.AdmissionReview) *admission.AdmissionResponse
	ValidateBackendGroupUpdate(*admission.AdmissionReview) *admission.AdmissionResponse
	ValidateBackendGroupDelete(*admission.AdmissionReview) *admission.AdmissionResponse
}

type MutateAdmitWebhook interface {
	MutateLB(*admission.AdmissionReview) *admission.AdmissionResponse
	MutateDriver(*admission.AdmissionReview) *admission.AdmissionResponse
	MutateBackendGroup(*admission.AdmissionReview) *admission.AdmissionResponse
}

func NewAdmitter(lbLister lbcflister.LoadBalancerLister, driverLister lbcflister.LoadBalancerDriverLister, backendLister lbcflister.BackendRecordLister, invoker util.WebhookInvoker) AdmitWebhook {
	return &Admitter{
		lbLister:       lbLister,
		driverLister:   driverLister,
		backendLister:  backendLister,
		webhookInvoker: invoker,
	}
}

type Admitter struct {
	lbLister      lbcflister.LoadBalancerLister
	driverLister  lbcflister.LoadBalancerDriverLister
	backendLister lbcflister.BackendRecordLister

	webhookInvoker util.WebhookInvoker
}

func (a *Admitter) MutateLB(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	obj := &lbcfapi.LoadBalancer{}
	err := json.Unmarshal(ar.Request.Object.Raw, obj)
	if err != nil {
		return toAdmissionResponse(err)
	}
	reviewResponse := &admission.AdmissionResponse{}
	reviewResponse.Allowed = true
	reviewResponse.Patch = util.MakeFinalizerPatch(len(obj.Finalizers) == 0, lbcfapi.FinalizerDeleteLB)
	pt := admission.PatchTypeJSONPatch
	reviewResponse.PatchType = &pt
	return reviewResponse
}

func (a *Admitter) MutateDriver(*admission.AdmissionReview) *admission.AdmissionResponse {
	return toAdmissionResponse(nil)
}

func (a *Admitter) MutateBackendGroup(*admission.AdmissionReview) *admission.AdmissionResponse {
	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateLoadBalancerCreate(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	lb := &lbcfapi.LoadBalancer{}
	if err := json.Unmarshal(ar.Request.Object.Raw, lb); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancer failed: %v", err))
	}

	errList := ValidateLoadBalancer(lb)
	if len(errList) > 0 {
		return toAdmissionResponse(fmt.Errorf("%s", errList.ToAggregate().Error()))
	}

	driverNamespace := util.GetDriverNamespace(lb.Spec.LBDriver, lb.Namespace)
	driver, err := a.driverLister.LoadBalancerDrivers(driverNamespace).Get(lb.Spec.LBDriver)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("retrieve driver %s/%s failed: %v", driverNamespace, lb.Spec.LBDriver, err))
	}
	if util.IsDriverDraining(driver.Labels) {
		return toAdmissionResponse(fmt.Errorf("driver %q is draining, all LoadBalancer creating operation for that dirver is denied", lb.Spec.LBDriver))
	} else if driver.DeletionTimestamp != nil {
		return toAdmissionResponse(fmt.Errorf("driver %q is deleting, all LoadBalancer creating operation for that dirver is denied", lb.Spec.LBDriver))
	}
	req := &webhooks.ValidateLoadBalancerRequest{
		LBSpec:     lb.Spec.LBSpec,
		Operation:  webhooks.OperationCreate,
		Attributes: lb.Spec.Attributes,
	}
	rsp, err := a.webhookInvoker.CallValidateLoadBalancer(driver, req)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("call webhook error, webhook: validateLoadBalancer, err: %v", err))
	} else if !rsp.Succ {
		return toAdmissionResponse(fmt.Errorf("invalid LoadBalancer: %s", rsp.Msg))
	}

	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateLoadBalancerUpdate(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	curObj := &lbcfapi.LoadBalancer{}
	oldObj := &lbcfapi.LoadBalancer{}

	if err := json.Unmarshal(ar.Request.Object.Raw, curObj); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancer failed: %v", err))
	}
	if err := json.Unmarshal(ar.Request.OldObject.Raw, oldObj); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancer failed: %v", err))
	}
	if !LBUpdatedFieldsAllowed(curObj, oldObj) {
		return toAdmissionResponse(fmt.Errorf("update to non-attributes fields is not permitted"))
	}

	errList := ValidateLoadBalancer(curObj)
	if len(errList) > 0 {
		return toAdmissionResponse(fmt.Errorf("%s", errList.ToAggregate().Error()))
	}

	// if LoadBalancer.status is the only updated field, return immediately
	if !util.LoadBalancerNonStatusUpdated(oldObj, curObj) {
		return toAdmissionResponse(nil)
	}

	driverNamespace := util.GetDriverNamespace(curObj.Spec.LBDriver, curObj.Namespace)
	driver, err := a.driverLister.LoadBalancerDrivers(driverNamespace).Get(curObj.Spec.LBDriver)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("retrieve driver %s/%s failed: %v", driverNamespace, curObj.Spec.LBDriver, err))
	}

	req := &webhooks.ValidateLoadBalancerRequest{
		LBSpec:        curObj.Spec.LBSpec,
		Operation:     webhooks.OperationUpdate,
		Attributes:    curObj.Spec.Attributes,
		OldAttributes: oldObj.Spec.Attributes,
	}
	rsp, err := a.webhookInvoker.CallValidateLoadBalancer(driver, req)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("call webhook error, webhook: validateLoadBalancer, err: %v", err))
	} else if !rsp.Succ {
		return toAdmissionResponse(fmt.Errorf("invalid LoadBalancer: %s", rsp.Msg))
	}

	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateLoadBalancerDelete(*admission.AdmissionReview) *admission.AdmissionResponse {
	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateDriverCreate(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	klog.Infof("start ValidateDriverCreate")
	d := &lbcfapi.LoadBalancerDriver{}
	if err := json.Unmarshal(ar.Request.Object.Raw, d); err != nil {
		klog.Errorf(err.Error())
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancerDriver failed: %v", err))
	}

	errList := ValidateLoadBalancerDriver(d)
	if len(errList) > 0 {
		return toAdmissionResponse(fmt.Errorf("%s", errList.ToAggregate().Error()))
	}

	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateDriverUpdate(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	curObj := &lbcfapi.LoadBalancerDriver{}
	oldObj := &lbcfapi.LoadBalancerDriver{}

	if err := json.Unmarshal(ar.Request.Object.Raw, curObj); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancerDriver failed: %v", err))
	}
	if err := json.Unmarshal(ar.Request.OldObject.Raw, oldObj); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancerDriver failed: %v", err))
	}

	if !DriverUpdatedFieldsAllowed(curObj, oldObj) {
		return toAdmissionResponse(fmt.Errorf("update to LoadBalancerUpdate is not permitted"))
	}
	errList := ValidateLoadBalancerDriver(curObj)
	if len(errList) > 0 {
		return toAdmissionResponse(fmt.Errorf("%s", errList.ToAggregate().Error()))
	}
	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateDriverDelete(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	driver, err := a.driverLister.LoadBalancerDrivers(ar.Request.Namespace).Get(ar.Request.Name)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("retrieve LoadBalancerDriver %s/%s failed: %v", ar.Request.Namespace, ar.Request.Name, err))
	}
	if !util.IsDriverDraining(driver.Labels) {
		return toAdmissionResponse(fmt.Errorf("LoadBalancerDriver must be label with %s:\"true\" before delete", lbcfapi.DriverDrainingLabel))
	}

	lbList, err := a.listLoadBalancerByDriver(ar.Request.Name, ar.Request.Namespace)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("unable to list LoadBalancers for driver, err: %v", err))
	} else if len(lbList) > 0 {
		return toAdmissionResponse(fmt.Errorf("all LoadBalancers must be deleted, %d remaining", len(lbList)))
	}

	beList, err := a.listBackendByDriver(ar.Request.Name, ar.Request.Namespace)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("unable to list BackendRecords for driver, err: %v", err))
	} else if len(beList) > 0 {
		return toAdmissionResponse(fmt.Errorf("all BackendRecord must be deregistered, %d remaining", len(beList)))
	}
	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateBackendGroupCreate(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	bg := &lbcfapi.BackendGroup{}
	if err := json.Unmarshal(ar.Request.Object.Raw, bg); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode BackendGroup failed: %v", err))
	}
	errList := ValidateBackendGroup(bg)
	if len(errList) > 0 {
		return toAdmissionResponse(fmt.Errorf("%s", errList.ToAggregate().Error()))
	}

	lb, err := a.lbLister.LoadBalancers(bg.Namespace).Get(bg.Spec.LBName)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("loadbalancer not found, LoadBalancer must be created before BackendGroup"))
	}
	if lb.DeletionTimestamp != nil {
		return toAdmissionResponse(fmt.Errorf("operation denied: loadbalancer %q is deleting", lb.Name))
	}
	driverNamespace := util.GetDriverNamespace(lb.Spec.LBDriver, bg.Namespace)
	driver, err := a.driverLister.LoadBalancerDrivers(driverNamespace).Get(lb.Spec.LBDriver)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("retrieve driver %s/%s failed: %v", driverNamespace, lb.Spec.LBDriver, err))
	}
	req := &webhooks.ValidateBackendRequest{
		BackendType: string(util.GetBackendType(bg)),
		LBInfo:      lb.Status.LBInfo,
		Operation:   webhooks.OperationCreate,
		Parameters:  bg.Spec.Parameters,
	}
	rsp, err := a.webhookInvoker.CallValidateBackend(driver, req)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("call webhook error, webhook validateBackend, err: %v", err))
	} else if !rsp.Succ {
		return toAdmissionResponse(fmt.Errorf("invalid Backend, err: %v", err))
	}

	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateBackendGroupUpdate(ar *admission.AdmissionReview) *admission.AdmissionResponse {
	curObj := &lbcfapi.BackendGroup{}
	oldObj := &lbcfapi.BackendGroup{}

	if err := json.Unmarshal(ar.Request.Object.Raw, curObj); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancerDriver failed: %v", err))
	}
	if err := json.Unmarshal(ar.Request.OldObject.Raw, oldObj); err != nil {
		return toAdmissionResponse(fmt.Errorf("decode LoadBalancerDriver failed: %v", err))
	}

	if !BackendGroupUpdateFieldsAllowed(curObj, oldObj) {
		return toAdmissionResponse(fmt.Errorf("update to backend type is not permitted"))
	}

	errList := ValidateBackendGroup(curObj)
	if len(errList) > 0 {
		return toAdmissionResponse(fmt.Errorf("%s", errList.ToAggregate().Error()))
	}

	// if BackendGroup.status is the only updated field, return immediately
	if !util.BackendGroupNonStatusUpdated(oldObj, curObj) {
		return toAdmissionResponse(nil)
	}

	lb, err := a.lbLister.LoadBalancers(curObj.Namespace).Get(curObj.Spec.LBName)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("loadbalancer not found, LoadBalancer must be created before BackendGroup"))
	}
	driverNamespace := util.GetDriverNamespace(lb.Spec.LBDriver, curObj.Namespace)
	driver, err := a.driverLister.LoadBalancerDrivers(driverNamespace).Get(lb.Spec.LBDriver)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("retrieve driver %s/%s failed: %v", driverNamespace, lb.Spec.LBDriver, err))
	}

	req := &webhooks.ValidateBackendRequest{
		BackendType: string(util.GetBackendType(curObj)),
		LBInfo:      lb.Status.LBInfo,
		Operation:   webhooks.OperationUpdate,
		Parameters:  curObj.Spec.Parameters,
	}
	rsp, err := a.webhookInvoker.CallValidateBackend(driver, req)
	if err != nil {
		return toAdmissionResponse(fmt.Errorf("call webhook error, webhook validateBackend, err: %v", err))
	} else if !rsp.Succ {
		return toAdmissionResponse(fmt.Errorf("invalid Backend, err: %v", err))
	}
	return toAdmissionResponse(nil)
}

func (a *Admitter) ValidateBackendGroupDelete(*admission.AdmissionReview) *admission.AdmissionResponse {
	return toAdmissionResponse(nil)
}

func (a *Admitter) listLoadBalancerByDriver(driverName string, driverNamespace string) ([]*lbcfapi.LoadBalancer, error) {
	lbList, err := a.lbLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	var ret []*lbcfapi.LoadBalancer
	for _, lb := range lbList {
		if driverNamespace != "kube-system" && lb.Namespace != driverNamespace {
			continue
		}
		if lb.Spec.LBDriver == driverName {
			ret = append(ret, lb)
		}
	}
	return ret, nil
}

func (a *Admitter) listBackendByDriver(driverName string, driverNamespace string) ([]*lbcfapi.BackendRecord, error) {
	recordList, err := a.backendLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	var ret []*lbcfapi.BackendRecord
	for _, r := range recordList {
		if driverNamespace != "kube-system" && r.Namespace != driverNamespace {
			continue
		}
		if r.Spec.LBDriver == driverName {
			ret = append(ret, r)
		}
	}
	return ret, nil
}