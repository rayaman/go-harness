package bdo

import (
	"fmt"
	"strings"
)

type SubCategory struct {
	ID   int
	Name string
}

type MainCategory struct {
	ID            int
	Name          string
	SubCategories []SubCategory
}

var categories = []MainCategory{
	{1, "Main Weapon", []SubCategory{
		{1, "Longsword"}, {2, "Longbow"}, {3, "Amulet"}, {4, "Axe"}, {5, "Shortsword"},
		{6, "Blade"}, {7, "Staff"}, {8, "Kriegsmesser"}, {9, "Gauntlet"}, {10, "Crescent Pendulum"},
		{11, "Crossbow"}, {12, "Florang"}, {13, "Battle Axe"}, {14, "Shamshir"}, {15, "Morning Star"},
		{16, "Kyve"}, {17, "Serenaca"}, {18, "Slayer"}, {19, "Swallowtail Fan"}, {20, "Foxspirit Charm"},
		{21, "Hammers"}, {22, "Hwando"}, {23, "Revolvers"}, {24, "Power Pole"}, {25, "Sacramentum"},
	}},
	{5, "Sub-weapon", []SubCategory{
		{1, "Shield"}, {2, "Dagger"}, {3, "Talisman"}, {4, "Ornamental Knot"}, {5, "Trinket"},
		{6, "Horn Bow"}, {7, "Kunai"}, {8, "Shuriken"}, {9, "Vambrace"}, {10, "Noble Sword"},
		{11, "Ra'ghon"}, {12, "Vitclari"}, {13, "Haladie"}, {14, "Quoratum"}, {15, "Mareca"},
		{16, "Shard"}, {17, "Do Stave"}, {18, "Binyeo Knife"}, {19, "Gravity Cores"}, {20, "Gombangdae"},
		{21, "Shotgun"}, {22, "Gourd Bottle"}, {23, "Clavis"}, {24, "Hexround"},
	}},
	{10, "Awakening", []SubCategory{
		{1, "Greatsword"}, {2, "Scythe"}, {3, "Iron Buster"}, {4, "Kamasylven Sword"}, {5, "Celestial Bo Staff"},
		{6, "Lancia"}, {7, "Crescent Blade"}, {8, "Kerispear"}, {9, "Sura Katana"}, {10, "Sah Chakram"},
		{11, "Aad Sphera"}, {12, "Godr Sphera"}, {13, "Vediant"}, {14, "Gardbrace"}, {15, "Cestus"},
		{16, "Crimson Glaives"}, {17, "Greatbow"}, {19, "Jordun"}, {20, "Dual Glaives"}, {21, "Sting"},
		{22, "Kibelius"}, {23, "Patraca"}, {24, "Trion"}, {25, "Soul Tome"}, {26, "Foxtail Fans"},
		{27, "Sledgehammer"}, {28, "Enlightened Blade"}, {29, "Lil' Devil"}, {30, "Jade Pillar"}, {31, "Purgatum"},
	}},
	{15, "Armor", []SubCategory{
		{1, "Helmet"}, {2, "Armor"}, {3, "Gloves"}, {4, "Shoes"}, {5, "Functional Clothes"}, {6, "Crafted Clothes"},
	}},
	{20, "Accessories", []SubCategory{
		{1, "Ring"}, {2, "Necklace"}, {3, "Earring"}, {4, "Belt"},
	}},
	{25, "Material", []SubCategory{
		{1, "Ore/Gem"}, {2, "Plants"}, {3, "Seed"}, {4, "Fruit"}, {5, "Vegetable"},
		{6, "Grain"}, {7, "Seafood"}, {8, "Meat"}, {9, "Hide"}, {10, "Blood"}, {11, "Misc."},
	}},
	{30, "Enhancement", []SubCategory{
		{1, "Black Stone"}, {2, "Upgrade"}, {3, "Reforge"},
	}},
	{35, "Consumables", []SubCategory{
		{1, "Offensive Elixir"}, {2, "Defensive Elixir"}, {3, "Functional Elixir"}, {4, "Food"},
		{5, "Unique Food"}, {6, "Potion"}, {7, "Siege Items"}, {8, "Item Parts"}, {9, "Other Consumables"},
	}},
	{40, "Life Tools", []SubCategory{
		{1, "Lumbering Axe"}, {2, "Fluid Collector"}, {3, "Butcher Knife"}, {4, "Pickaxe"}, {5, "Hoe"},
		{6, "Tanning Knife"}, {7, "Fishing Tools"}, {8, "Matchlock"}, {9, "Alchemy/Cooking"}, {10, "Other Tools"},
	}},
	{45, "Alchemy Stone", []SubCategory{
		{1, "Destruction"}, {2, "Protection"}, {3, "Life"}, {4, "Spirit Stone"},
	}},
	{50, "Magic Crystal", []SubCategory{
		{1, "Attack Crystal"}, {2, "Defense Crystal"}, {3, "Functional Crystal"}, {4, "Combined Crystal"},
		{5, "Gloves"}, {6, "Shoes"}, {7, "Versatile"}, {8, "Awakening Weapon"},
	}},
	{55, "Pearl Item", []SubCategory{
		{1, "Male Apparel (Set)"}, {2, "Female Apparel (Set)"}, {3, "Male Apparel (Individual)"},
		{4, "Female Apparel (Individual)"}, {5, "Class-based Apparel (Set)"}, {6, "Functional"},
		{7, "Mount"}, {8, "Pet"},
	}},
	{60, "Dye", []SubCategory{
		{1, "Basic"}, {2, "Olvia"}, {3, "Velia"}, {4, "Heidelian"}, {5, "Keplan"},
		{6, "Calpheon"}, {7, "Mediah"}, {8, "Valencia"}, {9, "Effect"},
	}},
	{65, "Mount", []SubCategory{
		{1, "Registration"}, {2, "Feed"}, {3, "Champron"}, {4, "Barding"}, {5, "Saddle"},
		{6, "Stirrups"}, {7, "Horseshoe"}, {9, "[Elephant] Stirrups"}, {10, "[Elephant] Armor"},
		{11, "[Elephant] Mask"}, {12, "[Elephant] Saddle"}, {13, "Courser Training"},
	}},
	{70, "Ship", []SubCategory{
		{1, "License"}, {2, "Cargo"}, {3, "Prow"}, {4, "Decoration"}, {5, "Totem"},
		{6, "Figurehead"}, {7, "Plating"}, {8, "Cannon"}, {9, "Sail"},
	}},
	{75, "Wagon", []SubCategory{
		{1, "Registration"}, {2, "Wheel"}, {3, "Cover"}, {4, "Flag"}, {5, "Emblem"}, {6, "Lamp"},
	}},
	{80, "Furniture", []SubCategory{
		{1, "Bed"}, {2, "Bedside Table/Table"}, {3, "Wardrobe/Bookshelf"}, {4, "Sofa/Chair"},
		{5, "Chandelier"}, {6, "Floor/Carpet"}, {7, "Wall/Curtain"}, {8, "Decoration"}, {9, "Others"},
	}},
	{85, "Artifact/Lightstone", []SubCategory{
		{1, "Lightstone of Fire"}, {2, "Lightstone of Earth"}, {3, "Lightstone of Wind"},
		{4, "Lightstone of Flora"}, {5, "Special Lightstone"}, {6, "Artifact"},
	}},
}

// GetCategoryNames takes a mainCategory ID and a subCategory ID and returns
// the main category name and sub category name. If either ID is not found,
// an error is returned instead.
func GetCategoryNames(mainCategory, subCategory int) (string, string, error) {
	for _, mc := range categories {
		if mc.ID == mainCategory {
			for _, sc := range mc.SubCategories {
				if sc.ID == subCategory {
					return mc.Name, sc.Name, nil
				}
			}
			return "", "", fmt.Errorf("subCategory %d not found in mainCategory %d (%s)", subCategory, mainCategory, mc.Name)
		}
	}
	return "", "", fmt.Errorf("mainCategory %d not found", mainCategory)
}

func GetMainCategoryName(MainCategory int) (string, error) {
	for _, mc := range categories {
		if mc.ID == MainCategory {
			return mc.Name, nil
		}
	}
	return "", fmt.Errorf("mainCategory %d not found", MainCategory)
}

func GetCategoryIDs(mainCategoryName, subCategoryName string) (int, int, error) {
	for _, mc := range categories {
		if strings.EqualFold(mc.Name, mainCategoryName) {
			for _, sc := range mc.SubCategories {
				if strings.EqualFold(sc.Name, subCategoryName) {
					return mc.ID, sc.ID, nil
				}
			}
			return 0, 0, fmt.Errorf("subCategory %q not found in mainCategory %q", subCategoryName, mainCategoryName)
		}
	}
	return 0, 0, fmt.Errorf("mainCategory %q not found", mainCategoryName)
}

func GetMainCategoryID(mainCategoryName string) (int, error) {
	for _, mc := range categories {
		if strings.EqualFold(mc.Name, mainCategoryName) {
			return mc.ID, nil
		}
	}
	return 0, fmt.Errorf("mainCategory %q not found", mainCategoryName)
}
