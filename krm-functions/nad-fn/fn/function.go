/*
Copyright 2023 Nephio.

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

package fn

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleContainerTools/kpt-functions-sdk/go/fn"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	infrav1alpha1 "github.com/nephio-project/api/infra/v1alpha1"
	nephioreqv1alpha1 "github.com/nephio-project/api/nf_requirements/v1alpha1"
	"github.com/nephio-project/nephio/krm-functions/lib/condkptsdk"
	ko "github.com/nephio-project/nephio/krm-functions/lib/kubeobject"
	nadlibv1 "github.com/nephio-project/nephio/krm-functions/lib/nad/v1"
	ipamv1alpha1 "github.com/nokia/k8s-ipam/apis/resource/ipam/v1alpha1"
	vlanv1alpha1 "github.com/nokia/k8s-ipam/apis/resource/vlan/v1alpha1"
	"github.com/nokia/k8s-ipam/pkg/iputil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultPODNetwork = "default"

type nadFn struct {
	sdk             condkptsdk.KptCondSDK
	workloadCluster *infrav1alpha1.WorkloadCluster
	forName         string
	forNamespace    string
	networkObjs     []infrav1alpha1.Network
}

func Run(rl *fn.ResourceList) (bool, error) {
	myFn := nadFn{}
	var err error
	myFn.sdk, err = condkptsdk.New(
		rl,
		&condkptsdk.Config{
			For: corev1.ObjectReference{
				APIVersion: nadv1.SchemeGroupVersion.Identifier(),
				Kind:       reflect.TypeOf(nadv1.NetworkAttachmentDefinition{}).Name(),
			},
			Watch: map[corev1.ObjectReference]condkptsdk.WatchCallbackFn{
				{
					APIVersion: infrav1alpha1.GroupVersion.Identifier(),
					Kind:       infrav1alpha1.WorkloadClusterKind,
				}: myFn.WorkloadClusterCallbackFn,
				{
					APIVersion: infrav1alpha1.GroupVersion.Identifier(),
					Kind:       infrav1alpha1.NetworkKind,
				}: myFn.NetworkCallbackFn,
				{
					APIVersion: ipamv1alpha1.GroupVersion.Identifier(),
					Kind:       ipamv1alpha1.IPClaimKind,
				}: nil,
				{
					APIVersion: vlanv1alpha1.GroupVersion.Identifier(),
					Kind:       vlanv1alpha1.VLANClaimKind,
				}: nil,
				{
					APIVersion: nephioreqv1alpha1.GroupVersion.Identifier(),
					Kind:       nephioreqv1alpha1.InterfaceKind,
				}: nil,
			},
			PopulateOwnResourcesFn: nil,
			UpdateResourceFn:       myFn.updateResourceFn,
		},
	)
	if err != nil {
		rl.Results.ErrorE(err)
		return false, err
	}
	return myFn.sdk.Run()
}

// WorkloadClusterCallbackFn provides a callback for the workload cluster
// resources in the resourceList
func (f *nadFn) WorkloadClusterCallbackFn(o *fn.KubeObject) error {
	var err error

	if f.workloadCluster != nil {
		return fmt.Errorf("multiple WorkloadCluster objects found in the kpt package")
	}
	f.workloadCluster, err = ko.KubeObjectToStruct[infrav1alpha1.WorkloadCluster](o)
	if err != nil {
		return err
	}

	// validate check the specifics of the spec, like mandatory fields
	return f.workloadCluster.Spec.Validate()
}

func (f *nadFn) NetworkCallbackFn(o *fn.KubeObject) error {
	networkObj, err := ko.KubeObjectToStruct[infrav1alpha1.Network](o)
	if err != nil {
		return err
	}
	f.networkObjs = append(f.networkObjs, *networkObj)
	return nil
}

func (f *nadFn) updateResourceFn(_ *fn.KubeObject, objs fn.KubeObjects) (fn.KubeObjects, error) {
	if f.workloadCluster == nil {
		// no WorkloadCluster resource in the package
		return nil, fmt.Errorf("workload cluster is missing from the kpt package")
	}

	// the NAD needs a prefix equal to the owner of the deployment and it needs a namespace aligned with the deployment
	// Given we don't do the intelligent diff we need to look for the owner resource
	// With the intelligent diff this will be propagated via the annotations.
	interfaceObjs := objs.Where(fn.IsGroupVersionKind(nephioreqv1alpha1.InterfaceGroupVersionKind))
	if interfaceObjs.Len() == 0 {
		return nil, fmt.Errorf("expected %s object to generate the nad", nephioreqv1alpha1.InterfaceKind)
	}
	for _, o := range interfaceObjs {
		f.forName = getForName(o.GetAnnotations())
		f.forNamespace = o.GetAnnotation(condkptsdk.SpecializerNamespace)
		//fn.Logf("interface callback: kind: %s, name: %s, namespace: %s, annotations: %s\n", o.GetKind(), o.GetName(), o.GetNamespace(), o.GetAnnotations())
	}

	if f.forName == "" || f.forNamespace == "" {
		// no for name or for namespace present
		return nil, fmt.Errorf("expecting a for name and for namespace, got forName: %s, forNamespace: %s", f.forName, f.forNamespace)
	}

	ipClaimObjs := objs.Where(fn.IsGroupVersionKind(ipamv1alpha1.IPClaimGroupVersionKind))
	vlanClaimObjs := objs.Where(fn.IsGroupVersionKind(vlanv1alpha1.VLANClaimGroupVersionKind))

	fn.Logf("nad updateResourceFn: ifObj: %d, ipClaimObj: %d, vlanClaimObj: %d, networkObjs: %d\n", len(interfaceObjs), len(ipClaimObjs), len(vlanClaimObjs), len(f.networkObjs))

	itfceKOE, err := ko.NewFromKubeObject[nephioreqv1alpha1.Interface](interfaceObjs[0])
	if err != nil {
		return nil, err
	}

	itfce, err := itfceKOE.GetGoStruct()
	if err != nil {
		return nil, err
	}

	// nothing to be done
	if itfce.Spec.NetworkInstance.Name == defaultPODNetwork {
		return nil, nil
	}

	if ipClaimObjs.Len() == 0 && vlanClaimObjs.Len() == 0 {
		return nil, fmt.Errorf("expected one of %s or %s objects to generate the nad", ipamv1alpha1.IPClaimKind, vlanv1alpha1.VLANClaimKind)
	}

	// generate an empty nad struct
	nad, err := nadlibv1.NewFromGoStruct(&nadv1.NetworkAttachmentDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: nadv1.SchemeGroupVersion.Identifier(),
			Kind:       reflect.TypeOf(nadv1.NetworkAttachmentDefinition{}).Name(),
		},
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%s", f.forName, interfaceObjs[0].GetName()), Namespace: f.forNamespace},
	})
	if err != nil {
		return nil, err
	}

	if ipClaimObjs.Len() == 0 && vlanClaimObjs.Len() != 0 {
		nad.CniSpecType = nadlibv1.VlanClaimOnly
	}

	vlanID := 0
	for _, vlanClaim := range vlanClaimObjs {
		vlanID, _, _ = vlanClaim.NestedInt([]string{"status", "vlanID"}...)
	}

	if nad.CniSpecType != nadlibv1.VlanClaimOnly {
		for _, itfce := range interfaceObjs {
			i, err := ko.NewFromKubeObject[nephioreqv1alpha1.Interface](itfce)
			if err != nil {
				return nil, err
			}

			itfceGoStruct, err := i.GetGoStruct()
			if err != nil {
				return nil, err
			}

			if !f.IsCNITypePresent(itfceGoStruct.Spec.CNIType) {
				return nil, fmt.Errorf("cniType not supported in workload cluster; workload cluster CNI(s): %v, interface cniType requested: %s", f.workloadCluster.Spec.CNIs, itfceGoStruct.Spec.CNIType)
			}
			cniType := itfceGoStruct.Spec.CNIType

			if err := nad.SetCNIType(string(cniType)); err != nil {
				return nil, err
			}
			masterInterface := *f.workloadCluster.Spec.MasterInterface // since we validated the workload cluster before it is safe to do this
			if cniType != "vlan" && vlanID != 0 {
				masterInterface = fmt.Sprintf("%s.%s", masterInterface, strconv.Itoa(vlanID))
			}
			if cniType != "bridge" {
				err = nad.SetNadMaster(masterInterface)
				if err != nil {
					return nil, err
				}
			} else {
				err = nad.SetBridgeName(vlanID)
				if err != nil {
					return nil, err
				}
			}
		}

		var nadAddresses []nadlibv1.Address
		var nadRoutes []nadlibv1.Route
		for _, ipClaim := range ipClaimObjs {
			claim, err := ko.NewFromKubeObject[ipamv1alpha1.IPClaim](ipClaim)
			if err != nil {
				return nil, err
			}

			ipclaimGoStruct, err := claim.GetGoStruct()
			if err != nil {
				return nil, err
			}
			address := ""
			gateway := ""
			if ipclaimGoStruct.Status.Prefix != nil {
				address = *ipclaimGoStruct.Status.Prefix
			}
			if ipclaimGoStruct.Status.Gateway != nil {
				gateway = *ipclaimGoStruct.Status.Gateway
			}
			if !containsAddress(nadAddresses, address) {
				nadAddresses = append(nadAddresses, nadlibv1.Address{
					Address: address,
					Gateway: gateway,
				})
			}

			if address != "" && gateway != "" {
				for _, networkObj := range f.networkObjs {
					for _, rt := range networkObj.Spec.RoutingTables {
						if rt.Name == ipclaimGoStruct.Spec.NetworkInstance.Name {
							for _, prefix := range rt.Prefixes {
								pi, err := iputil.New(prefix.Prefix)
								if err != nil {
									return nil, err
								}
								pia, err := iputil.New(address)
								if err != nil {
									return nil, err
								}
								if pi.GetAddressFamily().String() == pia.GetAddressFamily().String() {
									if !containsDestination(nadRoutes, prefix.Prefix) {
										nadRoutes = append(nadRoutes, nadlibv1.Route{Destination: prefix.Prefix, Gateway: gateway})
									}
								}
							}
						}
					}
				}
			}
		}
		sort.Slice(nadAddresses, func(i, j int) bool {
			return nadAddresses[i].Address < nadAddresses[j].Address
		})
		err = nad.SetIpamAddress(nadAddresses)
		if err != nil {
			return nil, err
		}
		sort.Slice(nadRoutes, func(i, j int) bool {
			return nadRoutes[i].Destination < nadRoutes[j].Destination
		})
		err = nad.SetIpamRoutes(nadRoutes)
		if err != nil {
			return nil, err
		}
	}

	return fn.KubeObjects{&nad.K.KubeObject}, nil
}

func (f *nadFn) IsCNITypePresent(itfceCNIType nephioreqv1alpha1.CNIType) bool {
	for _, cni := range f.workloadCluster.Spec.CNIs {
		if nephioreqv1alpha1.CNIType(cni) == itfceCNIType {
			return true
		}
	}
	return false
}

func containsAddress(s []nadlibv1.Address, e string) bool {
	for _, a := range s {
		if a.Address == e {
			return true
		}
	}
	return false
}

func containsDestination(s []nadlibv1.Route, e string) bool {
	for _, a := range s {
		if a.Destination == e {
			return true
		}
	}
	return false
}

func getForName(annotations map[string]string) string {
	// forName is the resource that is the root resource of the specialization
	// e.g. UPFDeployment, SMFDeployment, AMFDeployment
	forFullName := annotations[condkptsdk.SpecializerOwner]
	if owner, ok := annotations[condkptsdk.SpecializerFor]; ok {
		forFullName = owner
	}
	split := strings.Split(forFullName, ".")
	return split[len(split)-1]
}
