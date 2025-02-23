/*
Copyright 2021-2023 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package controllers

import (
	"fmt"
	l "github.com/redhat-appstudio/build-service/pkg/logs"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	applicationapi "github.com/redhat-appstudio/application-api/api/v1alpha1"
	tektonapi "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	"context"
	"strings"
	"time"
	//+kubebuilder:scaffold:imports
)

const (
	UserNamespace = "user1-tenant"
	BaseComponent = "base-component"
	Operator1     = "operator1"
	Operator2     = "operator2"
	ImageUrl      = "IMAGE_URL"
	ImageDigest   = "IMAGE_DIGEST"
)

var failures = 0

func failingDependencyUpdate(ctx context.Context, client client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder, downstreamComponents []applicationapi.Component, result *BuildResult) (immediateRetry bool, err error) {
	if failures == 0 {
		return DefaultTestDependenciesUpdate(ctx, client, scheme, eventRecorder, downstreamComponents, result)
	}
	failures = failures - 1
	return true, fmt.Errorf("failure")
}

func DefaultTestDependenciesUpdate(ctx context.Context, client client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder, downstreamComponents []applicationapi.Component, result *BuildResult) (immediateRetry bool, err error) {
	log := ctrllog.FromContext(ctx)
	for _, downstreamComponent := range downstreamComponents {
		log.Info(fmt.Sprintf("Nudging %s due to successful build of %s", downstreamComponent.Name, result.Component.Name), l.Action, l.ActionUpdate)
		//TODO: do we want the event? It's just for unit testing at the moment
		eventRecorder.Event(&downstreamComponent, v1.EventTypeNormal, ComponentNudgedEventType, fmt.Sprintf("component %s.%s was nudged by successful build of %s that produces image %s:%s@%s", downstreamComponent.Namespace, downstreamComponent.Name, result.Component.Name, result.BuiltImageRepository, result.BuiltImageTag, result.Digest))

	}
	return false, nil
}

var _ = Describe("Component nudge controller", func() {

	delayTime = 0
	BeforeEach(func() {
		createNamespace(UserNamespace)
		baseComponentName := types.NamespacedName{Namespace: UserNamespace, Name: BaseComponent}
		createComponent(baseComponentName)
		createComponent(types.NamespacedName{Namespace: UserNamespace, Name: Operator1})
		createComponent(types.NamespacedName{Namespace: UserNamespace, Name: Operator2})
		baseComponent := applicationapi.Component{}
		err := k8sClient.Get(context.TODO(), baseComponentName, &baseComponent)
		Expect(err).ToNot(HaveOccurred())
		baseComponent.Spec.BuildNudgesRef = []string{Operator1, Operator2}
		err = k8sClient.Update(context.TODO(), &baseComponent)
		Expect(err).ToNot(HaveOccurred())

	})

	AfterEach(func() {
		failures = 0
		componentList := applicationapi.ComponentList{}
		err := k8sClient.List(context.TODO(), &componentList)
		Expect(err).ToNot(HaveOccurred())
		for i := range componentList.Items {
			_ = k8sClient.Delete(context.TODO(), &componentList.Items[i])
		}
		prList := tektonapi.PipelineRunList{}
		err = k8sClient.List(context.TODO(), &prList)
		Expect(err).ToNot(HaveOccurred())
		for i := range prList.Items {
			_ = k8sClient.Delete(context.TODO(), &prList.Items[i])
		}
		eventList := v1.EventList{}
		err = k8sClient.List(context.TODO(), &eventList)
		Expect(err).ToNot(HaveOccurred())
		for i := range eventList.Items {
			_ = k8sClient.Delete(context.TODO(), &eventList.Items[i])
		}
	})

	Context("Test build pipelines have finalizer behaviour", func() {
		It("Test finalizer added and removed on deletion", func() {
			createBuildPipelineRun("test-pipeline-1", UserNamespace, BaseComponent)
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
			pr := getPipelineRun("test-pipeline-1", UserNamespace)
			err := k8sClient.Delete(context.TODO(), pr)
			Expect(err).ToNot(HaveOccurred())
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return pr == nil
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())

		})

		It("Test finalizer removed on pipeline completion", func() {
			createBuildPipelineRun("test-pipeline-1", UserNamespace, BaseComponent)
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
			pr := getPipelineRun("test-pipeline-1", UserNamespace)
			pr.Status.SetCondition(&apis.Condition{
				Type:               apis.ConditionSucceeded,
				Status:             "False",
				LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now()}},
			})
			pr.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, pr)).Should(BeNil())
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return !controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())

		})
	})

	Context("Test build nudges component", func() {

		It("Test build performs nudge on success", func() {
			createBuildPipelineRun("test-pipeline-1", UserNamespace, BaseComponent)
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
			pr := getPipelineRun("test-pipeline-1", UserNamespace)
			pr.Status.SetCondition(&apis.Condition{
				Type:               apis.ConditionSucceeded,
				Status:             "True",
				LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now()}},
			})
			pr.Status.Results = []tektonapi.PipelineRunResult{
				{Name: ImageDigestParamName, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "sha256:12345"}},
				{Name: ImageUrl, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "quay.io.foo/bar:latest"}},
			}
			pr.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, pr)).Should(BeNil())
			Eventually(func() bool {
				events := v1.EventList{}
				err := k8sClient.List(context.TODO(), &events)
				Expect(err).ToNot(HaveOccurred())
				op1nudged := false
				op2nudged := false
				for _, i := range events.Items {
					if i.Reason == ComponentNudgedEventType && strings.Contains(i.Message, "quay.io.foo/bar:latest@sha256:12345") {
						if strings.Contains(i.Message, "operator1") {
							op1nudged = true
						}
						if strings.Contains(i.Message, "operator2") {
							op2nudged = true
						}
					}
				}
				return op1nudged && op2nudged
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
		})

		It("Test state pipeline not nudged", func() {
			createBuildPipelineRun("stale-pipeline", UserNamespace, BaseComponent)
			time.Sleep(time.Second + time.Millisecond)
			createBuildPipelineRun("test-pipeline-1", UserNamespace, BaseComponent)
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
			pr := getPipelineRun("test-pipeline-1", UserNamespace)
			pr.Status.SetCondition(&apis.Condition{
				Type:               apis.ConditionSucceeded,
				Status:             "True",
				LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now()}},
			})
			pr.Status.Results = []tektonapi.PipelineRunResult{
				{Name: ImageDigestParamName, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "sha256:12345"}},
				{Name: ImageUrl, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "quay.io.foo/bar:latest"}},
			}
			pr.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, pr)).Should(BeNil())
			Eventually(func() bool {
				events := v1.EventList{}
				err := k8sClient.List(context.TODO(), &events)
				Expect(err).ToNot(HaveOccurred())

				//test that the state pipeline run was marked as nudged, even though it has not completed
				stale := getPipelineRun("stale-pipeline", UserNamespace)
				if stale.Annotations == nil || stale.Annotations[NudgeProcessedAnnotationName] != "true" || controllerutil.ContainsFinalizer(stale, NudgeFinalizer) {
					return false
				}
				op1nudged := false
				op2nudged := false
				for _, i := range events.Items {
					if i.Reason == ComponentNudgedEventType && strings.Contains(i.Message, "quay.io.foo/bar:latest@sha256:12345") {
						if strings.Contains(i.Message, "operator1") {
							op1nudged = true
						}
						if strings.Contains(i.Message, "operator2") {
							op2nudged = true
						}
					}
				}
				return op1nudged && op2nudged
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
		})
	})

	Context("Test nudge failure handling", func() {

		It("Test single failure results in retry", func() {
			failures = 1
			createBuildPipelineRun("test-pipeline-1", UserNamespace, BaseComponent)
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
			pr := getPipelineRun("test-pipeline-1", UserNamespace)
			pr.Status.SetCondition(&apis.Condition{
				Type:               apis.ConditionSucceeded,
				Status:             "True",
				LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now()}},
			})
			pr.Status.Results = []tektonapi.PipelineRunResult{
				{Name: ImageDigestParamName, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "sha256:12345"}},
				{Name: ImageUrl, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "quay.io.foo/bar:latest"}},
			}
			pr.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, pr)).Should(BeNil())
			Eventually(func() bool {
				events := v1.EventList{}
				err := k8sClient.List(context.TODO(), &events)
				Expect(err).ToNot(HaveOccurred())
				op1nudged := false
				op2nudged := false
				failureRecorded := false
				for _, i := range events.Items {
					if i.Reason == ComponentNudgedEventType && strings.Contains(i.Message, "quay.io.foo/bar:latest@sha256:12345") {
						if strings.Contains(i.Message, "operator1") {
							op1nudged = true
						}
						if strings.Contains(i.Message, "operator2") {
							op2nudged = true
						}
					} else if i.Reason == ComponentNudgeFailedEventType {
						failureRecorded = true
					}
				}
				log.Info(fmt.Sprintf("Operator1 nudged: %v", op1nudged))
				log.Info(fmt.Sprintf("Operator2 nudged: %v", op2nudged))
				log.Info(fmt.Sprintf("failure recorded: %v", failureRecorded))
				return op1nudged && op2nudged && failureRecorded
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
		})

		It("Test retries exceeded", func() {
			failures = 10
			createBuildPipelineRun("test-pipeline-1", UserNamespace, BaseComponent)
			Eventually(func() bool {
				pr := getPipelineRun("test-pipeline-1", UserNamespace)
				return controllerutil.ContainsFinalizer(pr, NudgeFinalizer)
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
			pr := getPipelineRun("test-pipeline-1", UserNamespace)
			pr.Status.SetCondition(&apis.Condition{
				Type:               apis.ConditionSucceeded,
				Status:             "True",
				LastTransitionTime: apis.VolatileTime{Inner: metav1.Time{Time: time.Now()}},
			})
			pr.Status.Results = []tektonapi.PipelineRunResult{
				{Name: ImageDigestParamName, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "sha256:12345"}},
				{Name: ImageUrl, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "quay.io.foo/bar:latest"}},
			}
			pr.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, pr)).Should(BeNil())
			Eventually(func() bool {
				events := v1.EventList{}
				err := k8sClient.List(context.TODO(), &events)
				Expect(err).ToNot(HaveOccurred())
				op1nudged := false
				op2nudged := false
				failureCount := 0
				for _, i := range events.Items {
					if i.Reason == ComponentNudgedEventType && strings.Contains(i.Message, "quay.io.foo/bar:latest@sha256:12345") {
						if strings.Contains(i.Message, "operator1") {
							op1nudged = true
						}
						if strings.Contains(i.Message, "operator2") {
							op2nudged = true
						}
					} else if i.Reason == ComponentNudgeFailedEventType {
						failureCount++
					}
				}
				return !op1nudged && !op2nudged && failureCount == 3
			}, timeout, interval).WithTimeout(ensureTimeout).Should(BeTrue())
		})
	})

	Context("Test renovate config template", func() {
		// This test mostly just makes sure the template generates sane config
		It("test template output", func() {

			component := &applicationapi.Component{}
			component.Name = "test-component"
			buildResult := BuildResult{
				BuiltImageRepository:     "quay.io/sdouglas/multi-component-parent-image",
				BuiltImageTag:            "a8dce08dbdf290e5d616a83672ad3afcb4b455ef",
				Digest:                   "sha256:716be32f12f0dd31adbab1f57e9d0b87066e03de51c89dce8ffb397fbac92314",
				DistributionRepositories: []string{"registry.redhat.com/some-product"},
				Component:                component,
			}
			result, err := generateRenovateConfigForNudge("slug1", []renovateRepository{{Repository: "repo1", BaseBranches: []string{"main"}}}, &buildResult)
			println("!" + result + "!")
			Expect(err).Should(Succeed())
			Expect(strings.Contains(result, `a8dce08dbdf290e5d616a83672ad3afcb4b455ef`)).Should(BeTrue())
		})
	})
})

func getPipelineRun(name string, namespace string) *tektonapi.PipelineRun {
	pr := tektonapi.PipelineRun{}
	err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, &pr)
	if err != nil && errors.IsNotFound(err) {
		return nil
	}
	Expect(err).ToNot(HaveOccurred())
	return &pr
}

func createBuildPipelineRun(name string, namespace string, component string) *tektonapi.PipelineRun {
	pipelineSpec := &tektonapi.PipelineSpec{
		Results: []tektonapi.PipelineResult{
			{Name: ImageUrl, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "$(tasks.build-container.results.IMAGE_URL)"}},
			{Name: ImageDigest, Value: tektonapi.ResultValue{Type: tektonapi.ParamTypeString, StringVal: "$(tasks.build-container.results.IMAGE_DIGEST)"}},
		},
		Tasks: []tektonapi.PipelineTask{
			{
				Name: "build-container",
				TaskSpec: &tektonapi.EmbeddedTask{
					TaskSpec: tektonapi.TaskSpec{
						Results: []tektonapi.TaskResult{{Name: ImageUrl, Type: tektonapi.ResultsTypeString}, {Name: ImageDigest, Type: tektonapi.ResultsTypeString}},
						Steps: []tektonapi.Step{
							{
								Name:   "buildah",
								Image:  "quay.io/buildah/fakebuildaimage:latest",
								Script: "echo hello",
							},
						},
					},
				},
			},
		},
	}
	run := tektonapi.PipelineRun{}
	run.Labels = map[string]string{ComponentNameLabelName: component, PipelineRunTypeLabelName: PipelineRunBuildType}
	run.Annotations = map[string]string{PacEventTypeAnnotationName: PacEventPushType}
	run.Namespace = namespace
	run.Name = name
	run.Spec.PipelineSpec = pipelineSpec
	err := k8sClient.Create(context.TODO(), &run)
	Expect(err).ToNot(HaveOccurred())
	return &run
}
