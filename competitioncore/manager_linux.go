// Native manager proof reads a finite typed property list from root PID1 and
// binds the exact protected unit fragment. It never mutates systemd or caches.
package competitioncore

import (
	"context"
	"errors"
	"time"
)

// All requested objects/interfaces/members/properties are source-fixed. A peer
// cannot nominate another object and no control document extends this list.
type managerProperty struct{ iface, name, signature string }

func managerProperties() []managerProperty {
	return []managerProperty{
		{iface: unitInterface, name: "Id", signature: "s"},
		{iface: unitInterface, name: "LoadState", signature: "s"},
		{iface: unitInterface, name: "ActiveState", signature: "s"},
		{iface: unitInterface, name: "FragmentPath", signature: "s"},
		{iface: unitInterface, name: "DropInPaths", signature: "as"},
		{iface: sliceInterface, name: "ControlGroup", signature: "s"},
		{iface: sliceInterface, name: "MemoryMax", signature: "t"},
		{iface: sliceInterface, name: "MemorySwapMax", signature: "t"},
		{iface: sliceInterface, name: "TasksMax", signature: "t"},
		{iface: sliceInterface, name: "CPUQuotaPerSecUSec", signature: "t"},
		{iface: sliceInterface, name: "CPUQuotaPeriodUSec", signature: "t"},
		{iface: sliceInterface, name: "AllowedCPUs", signature: "ay"},
	}
}

// Peer identity is supplied by the kernel and unit bytes by held no-follow
// protected descriptors. Neither can be overridden outside package tests.
func nativeManagerProof(ctx context.Context, policy Policy, settings Settings) error {
	return verifyManager(ctx, policy, settings, connectNativeManager, func(pin Pin) error {
		content, err := readProtected(pin.Path, settings.DocumentBytes)
		if err != nil || digest(content) != pin.Sha256 {
			return errors.New("core handler unit fragment differs from its protected content pin")
		}
		return nil
	})
}

// Every proof creates a fresh deadline-bounded peer session and revalidates
// root unit bytes around it. Cancellation closes in-flight I/O immediately.
func verifyManager(parent context.Context, policy Policy, settings Settings, connect func(context.Context) (managerStream, error), fragment func(Pin) error) error {
	if validateSettings(settings) != nil || validatePolicy(policy, settings) != nil || connect == nil || fragment == nil {
		return errors.New("manager proof lacks its exact finite protected policy")
	}
	ctx, cancel := context.WithTimeout(parent, settings.Manager.Timeout)
	defer cancel()
	if ctx.Err() != nil || fragment(policy.HandlerUnit) != nil {
		return errors.New("manager proof has no live pinned handler fragment")
	}
	stream, err := connect(ctx)
	if err != nil {
		return errors.New("private manager connection unavailable")
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		stream.Close()
	})
	defer func() {
		if stop() {
			stream.Close()
		} else {
			// A false stop does not join an already-running callback. Its
			// Close owns the stream until it returns; do not race it or
			// publish completed proof/cleanup while it still owns work.
			<-callbackDone
		}
	}()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) || stream.SetDeadline(deadline) != nil {
		return errors.New("manager connection has no original finite I/O deadline")
	}
	bus := &managerBus{stream: stream, settings: settings.Manager, ctx: ctx}
	if err := bus.authenticate(); err != nil {
		return err
	}
	if err := bus.unit(); err != nil {
		return err
	}
	for _, property := range managerProperties() {
		body := managerText(managerText(nil, property.iface), property.name)
		content, err := bus.call(managerUnitObject, propertyInterface, "Get", "ss", body, "v")
		if err != nil {
			return err
		}
		decoder := &managerDecoder{content: content, limit: settings.Manager}
		value, err := decoder.variant(property.signature)
		if err != nil || decoder.offset != len(content) || validateManagerProperty(property, value, policy, settings) != nil {
			return errors.New("manager handler property is incomplete, wrongly typed or outside its fixed class")
		}
	}
	if err := bus.unit(); err != nil {
		return err
	}
	peer, err := stream.Peer()
	if err != nil || peer.uid != 0 || peer.pid != 1 || fragment(policy.HandlerUnit) != nil || ctx.Err() != nil || stream.Pending(settings.Manager.ControlBytes) != nil {
		return errors.New("manager peer, unit fragment or protocol custody changed during proof")
	}
	return nil
}

// Values are bus-native uint64 microseconds, not systemctl display text. A
// correct memory cap cannot hide wrong manager CPU, swap, task or unit state.
func validateManagerProperty(property managerProperty, value managerValue, policy Policy, settings Settings) error {
	if value.signature != property.signature {
		return errors.New("unexpected manager property type")
	}
	switch property.name {
	case "Id":
		if value.text == HandlerSliceName {
			return nil
		}
	case "LoadState":
		if value.text == "loaded" {
			return nil
		}
	case "ActiveState":
		if value.text == "active" {
			return nil
		}
	case "FragmentPath":
		if value.text == policy.HandlerUnit.Path && value.text == managerUnitPath {
			return nil
		}
	case "DropInPaths":
		if len(value.strings) == 0 {
			return nil
		}
	case "ControlGroup":
		if value.text == HandlerCgroup {
			return nil
		}
	case "MemoryMax":
		if value.number == settings.HandlerMemoryBytes {
			return nil
		}
	case "MemorySwapMax":
		if value.number == 0 {
			return nil
		}
	case "TasksMax":
		if value.number == settings.HandlerTasksMaximum {
			return nil
		}
	case "CPUQuotaPerSecUSec":
		if value.number == settings.HandlerCpuQuota/settings.HandlerCpuPeriod*1000000 {
			return nil
		}
	case "CPUQuotaPeriodUSec":
		if value.number == settings.HandlerCpuPeriod {
			return nil
		}
	case "AllowedCPUs":
		wanted, err := cpuSet(policy.ManagementCpuSet, settings.MaximumCpuIndex)
		if err != nil {
			return err
		}
		seen := 0
		for index, bits := range value.bytes {
			for bit := 0; bit < 8; bit++ {
				if bits&(1<<bit) != 0 {
					cpu := uint64(index*8 + bit)
					if cpu > uint64(settings.MaximumCpuIndex) || !wanted[cpu] {
						return errors.New("manager CPU mask escaped its exact management set")
					}
					seen++
				}
			}
		}
		if seen == len(wanted) {
			return nil
		}
	}
	return errors.New("manager property differs from the frozen handler class")
}
