package ippool

import (
	"fmt"
	"net"

	"github.com/containernetworking/plugins/plugins/ipam/host-local/backend/allocator"
	"github.com/harvester/webhook/pkg/server/admission"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	lbv1 "github.com/harvester/harvester-load-balancer/pkg/apis/loadbalancer.harvesterhci.io/v1beta1"
	ctllbv1 "github.com/harvester/harvester-load-balancer/pkg/generated/controllers/loadbalancer.harvesterhci.io/v1beta1"
	"github.com/harvester/harvester-load-balancer/pkg/ipam"
	"github.com/harvester/harvester-load-balancer/pkg/utils"
)

type ipPoolValidator struct {
	admission.DefaultValidator
	ipPoolCache ctllbv1.IPPoolCache
}

var _ admission.Validator = &ipPoolValidator{}

func NewIPPoolValidator(ipPoolCache ctllbv1.IPPoolCache) admission.Validator {
	return &ipPoolValidator{
		ipPoolCache: ipPoolCache,
	}
}

func (i *ipPoolValidator) Create(_ *admission.Request, newObj runtime.Object) error {
	pool := newObj.(*lbv1.IPPool)
	if len(pool.Spec.Ranges) == 0 {
		return fmt.Errorf(createErr, pool.Name, fmt.Errorf("range can't be empty"))
	}

	rs, err := ipam.LBRangesToAllocatorRangeSet(pool.Spec.Ranges)
	if err != nil {
		return fmt.Errorf(createErr, pool.Name, err)
	}

	others, err := i.getOtherPoolsRanges(pool.Name)
	if err != nil {
		return fmt.Errorf(createErr, pool.Name, err)
	}

	if err := checkRange(rs, others...); err != nil {
		return fmt.Errorf(createErr, pool.Name, err)
	}

	if err := i.checkSelector(pool); err != nil {
		return fmt.Errorf(createErr, pool.Name, err)
	}

	return nil
}

func (i *ipPoolValidator) Update(_ *admission.Request, _, newObj runtime.Object) error {
	pool := newObj.(*lbv1.IPPool)

	if pool.DeletionTimestamp != nil {
		return nil
	}

	if len(pool.Spec.Ranges) == 0 {
		return fmt.Errorf(updateErr, pool.Name, fmt.Errorf("range can't be empty"))
	}

	rs, err := ipam.LBRangesToAllocatorRangeSet(pool.Spec.Ranges)
	if err != nil {
		return fmt.Errorf(updateErr, pool.Name, err)
	}

	others, err := i.getOtherPoolsRanges(pool.Name)
	if err != nil {
		return fmt.Errorf(updateErr, pool.Name, err)
	}

	if err := checkRange(rs, others...); err != nil {
		return fmt.Errorf(updateErr, pool.Name, err)
	}

	if err := checkAllocated(rs, pool.Status.Allocated); err != nil {
		return fmt.Errorf(updateErr, pool.Name, err)
	}

	if err := i.checkSelector(pool); err != nil {
		return fmt.Errorf(updateErr, pool.Name, err)
	}

	return nil
}

func (i *ipPoolValidator) Delete(_ *admission.Request, oldObj runtime.Object) error {
	pool := oldObj.(*lbv1.IPPool)

	if len(pool.Status.Allocated) != 0 {
		return fmt.Errorf("can't delete pool before releasing all the allocated IP")
	}

	return nil
}

func (i *ipPoolValidator) Resource() admission.Resource {
	return admission.Resource{
		Names:      []string{"ippools"},
		Scope:      admissionregv1.ClusterScope,
		APIGroup:   lbv1.SchemeGroupVersion.Group,
		APIVersion: lbv1.SchemeGroupVersion.Version,
		ObjectType: &lbv1.IPPool{},
		OperationTypes: []admissionregv1.OperationType{
			admissionregv1.Create,
			admissionregv1.Update,
			admissionregv1.Delete,
		},
	}
}

func (i *ipPoolValidator) getOtherPoolsRanges(myName string) ([]allocator.RangeSet, error) {
	pools, err := i.ipPoolCache.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	lengthOfPools := len(pools)
	if lengthOfPools == 0 {
		return nil, nil
	}

	rangSets := make([]allocator.RangeSet, 0, lengthOfPools)
	for _, p := range pools {
		if p.Name != myName {
			r, err := ipam.LBRangesToAllocatorRangeSet(p.Spec.Ranges)
			if err != nil {
				return nil, err
			}
			rangSets = append(rangSets, r)
		}
	}

	return rangSets, nil
}

func checkRange(r allocator.RangeSet, others ...allocator.RangeSet) error {
	// check overlaps among the ranges of rangeSet r
	for i, r1 := range r {
		for j, r2 := range r[i+1:] {
			if r1.Overlaps(&r[i+1+j]) {
				return fmt.Errorf("there are overlaps between range %+v and %+v", r1, r2)
			}
		}
	}

	// check overlaps with other rangeSet
	for i := range others {
		if r.Overlaps(&others[i]) {
			return fmt.Errorf("the ranges %+v overlap with other pools", r)
		}
	}

	return nil
}

func checkAllocated(rs allocator.RangeSet, allocated map[string]string) error {
	for ipStr := range allocated {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return fmt.Errorf("invalid ip string %s", ipStr)
		}

		if !rs.Contains(ip) {
			return fmt.Errorf("allocated IP %s is excluded", ipStr)
		}
	}

	return nil
}

// checkSelector checks if the selector is valid.
// It's allowed to create a global IP pool only when there is no global IP pool.
// When a pool checking scope overlaps with other pools, ignore the global IP pool.
func (i *ipPoolValidator) checkSelector(pool *lbv1.IPPool) error {
	if isGlobalIPPool(pool) {
		return i.checkGlobalIPPool(pool)
	}

	if err := checkSelectorItself(pool); err != nil {
		return fmt.Errorf("selector %+v is invalid: %w", pool.Spec.Selector, err)
	}

	return i.checkSelectorWithOthers(pool)
}

func (i *ipPoolValidator) checkGlobalIPPool(pool *lbv1.IPPool) error {
	pools, err := i.ipPoolCache.List(labels.Set(map[string]string{
		utils.KeyGlobalIPPool: utils.ValueTrue,
	}).AsSelector())
	if err != nil {
		return err
	}

	for _, p := range pools {
		if p.Name == pool.Name {
			continue
		}
		return fmt.Errorf("there is already a global IP pool: %s", p.Name)
	}

	return nil
}

func checkSelectorItself(pool *lbv1.IPPool) error {
	r := &ipam.Requirement{Network: pool.Spec.Selector.Network}
	lenOfSelectorScope := len(pool.Spec.Selector.Scope)

	for i, t := range pool.Spec.Selector.Scope {
		r.Project = t.Project
		r.Namespace = t.Namespace
		r.Cluster = t.GuestCluster
		s := lbv1.Selector{
			Priority: pool.Spec.Selector.Priority,
			Network:  pool.Spec.Selector.Network,
		}
		if i != lenOfSelectorScope-1 {
			s.Scope = pool.Spec.Selector.Scope[i+1:]
		}

		if ipam.NewMatcher(s).Matches(r) {
			return fmt.Errorf("scope overlaps")
		}
	}

	return nil
}

func (i *ipPoolValidator) checkSelectorWithOthers(pool *lbv1.IPPool) error {
	pools, err := i.ipPoolCache.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, p := range pools {
		// skip itself and global IP pool
		if p.Name == pool.Name || isGlobalIPPool(p) {
			continue
		}
		// priority could not be same if it's not zero
		if p.Spec.Selector.Priority != 0 && p.Spec.Selector.Priority == pool.Spec.Selector.Priority {
			return fmt.Errorf("the priority can't be the same as the pool %s", p.Name)
		} else if p.Spec.Selector.Priority == 0 && pool.Spec.Selector.Priority == 0 {
			// check the scope overlaps if both of them are zero
			r := &ipam.Requirement{
				Network: pool.Spec.Selector.Network,
			}
			for _, t := range pool.Spec.Selector.Scope {
				r.Project = t.Project
				r.Namespace = t.Namespace
				r.Cluster = t.GuestCluster
				if ipam.NewMatcher(p.Spec.Selector).Matches(r) {
					return fmt.Errorf("scope selector is same as the pool %s with priority 0 Project %v Namespace %v GuestCluster %v, set a different priority or scope", p.Name, t.Project, t.Namespace, t.GuestCluster)
				}
			}
		}
	}

	return nil
}
