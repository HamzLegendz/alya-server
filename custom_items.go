package main

import (
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/world"
)

// Shield implements the missing vanilla shield item.
type Shield struct{}

func (Shield) EncodeItem() (name string, meta int16) { return "minecraft:shield", 0 }
func (Shield) MaxCount() int { return 1 }
func (Shield) DurabilityInfo() item.DurabilityInfo {
	return item.DurabilityInfo{
		MaxDurability: 336,
		BrokenItem:    func() item.Stack { return item.Stack{} },
	}
}

// FishingRod implements the missing vanilla fishing rod item.
type FishingRod struct{}

func (FishingRod) EncodeItem() (name string, meta int16) { return "minecraft:fishing_rod", 0 }
func (FishingRod) MaxCount() int { return 1 }
func (FishingRod) DurabilityInfo() item.DurabilityInfo {
	return item.DurabilityInfo{
		MaxDurability: 64,
		BrokenItem:    func() item.Stack { return item.Stack{} },
	}
}

func init() {
	world.RegisterItem(Shield{})
	world.RegisterItem(FishingRod{})
}
