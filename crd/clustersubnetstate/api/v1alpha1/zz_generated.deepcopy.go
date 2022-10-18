//go:build !ignore_autogenerated
// +build !ignore_autogenerated

// Code generated by controller-gen. DO NOT EDIT.

package v1alpha1

import (
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ClusterSubnetState) DeepCopyInto(out *ClusterSubnetState) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ClusterSubnetState.
func (in *ClusterSubnetState) DeepCopy() *ClusterSubnetState {
	if in == nil {
		return nil
	}
	out := new(ClusterSubnetState)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *ClusterSubnetState) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ClusterSubnetStateList) DeepCopyInto(out *ClusterSubnetStateList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]ClusterSubnetState, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ClusterSubnetStateList.
func (in *ClusterSubnetStateList) DeepCopy() *ClusterSubnetStateList {
	if in == nil {
		return nil
	}
	out := new(ClusterSubnetStateList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *ClusterSubnetStateList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ClusterSubnetStateSpec) DeepCopyInto(out *ClusterSubnetStateSpec) {
	*out = *in
	out.Scaler = in.Scaler
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ClusterSubnetStateSpec.
func (in *ClusterSubnetStateSpec) DeepCopy() *ClusterSubnetStateSpec {
	if in == nil {
		return nil
	}
	out := new(ClusterSubnetStateSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *ClusterSubnetStateStatus) DeepCopyInto(out *ClusterSubnetStateStatus) {
	*out = *in
	out.Scaler = in.Scaler
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new ClusterSubnetStateStatus.
func (in *ClusterSubnetStateStatus) DeepCopy() *ClusterSubnetStateStatus {
	if in == nil {
		return nil
	}
	out := new(ClusterSubnetStateStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *Scaler) DeepCopyInto(out *Scaler) {
	*out = *in
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new Scaler.
func (in *Scaler) DeepCopy() *Scaler {
	if in == nil {
		return nil
	}
	out := new(Scaler)
	in.DeepCopyInto(out)
	return out
}
