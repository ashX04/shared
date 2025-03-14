package moodboard

import (
    "encoding/json"
    "time"
    "os"
    "path/filepath"
    "sync"
    "fmt"
)

// MoodBoard represents a collection of arranged items
type MoodBoard struct {
    ID          string         `json:"id"`
    Name        string         `json:"name"`
    Description string         `json:"description"`
    Created     time.Time      `json:"created"`
    Modified    time.Time      `json:"modified"`
    Items       []MoodBoardItem `json:"items"`
}

// MoodBoardItem represents an individual item in the moodboard
type MoodBoardItem struct {
    ID       string `json:"id"`
    FilePath string `json:"filePath"`
    X        int    `json:"x"`
    Y        int    `json:"y"`
    Width    int    `json:"width"`
    Height   int    `json:"height"`
    Rotation int    `json:"rotation"`
    ZIndex   int    `json:"zIndex"`
}

// Storage paths
const (
    MoodBoardDir     = "./moodboards"
    MoodBoardIndex   = "./moodboards/index.json"
    ThumbnailDir     = "./moodboards/thumbnails"
)

var (
    mu sync.RWMutex
)

// NewMoodBoard creates a new moodboard with the given name and description
func NewMoodBoard(name, description string) (*MoodBoard, error) {
    id := fmt.Sprintf("%d", time.Now().UnixNano())
    now := time.Now()
    
    return &MoodBoard{
        ID:          id,
        Name:        name,
        Description: description,
        Created:     now,
        Modified:    now,
        Items:       make([]MoodBoardItem, 0),
    }, nil
}

// Save persists the moodboard to disk
func (m *MoodBoard) Save() error {
    mu.Lock()
    defer mu.Unlock()

    if err := os.MkdirAll(MoodBoardDir, 0755); err != nil {
        return err
    }

    m.Modified = time.Now()
    data, err := json.MarshalIndent(m, "", "  ")
    if err != nil {
        return err
    }

    return os.WriteFile(filepath.Join(MoodBoardDir, m.ID+".json"), data, 0644)
}

// AddItem adds a new item to the moodboard
func (m *MoodBoard) AddItem(filePath string, x, y, width, height int) error {
    item := MoodBoardItem{
        ID:       fmt.Sprintf("item_%d", time.Now().UnixNano()),
        FilePath: filePath,
        X:        x,
        Y:        y,
        Width:    width,
        Height:   height,
        ZIndex:   len(m.Items), // Place on top
    }
    
    m.Items = append(m.Items, item)
    return m.Save()
}

// UpdateItem updates an existing item's position and properties
func (m *MoodBoard) UpdateItem(id string, x, y, width, height, rotation, zIndex int) error {
    for i := range m.Items {
        if m.Items[i].ID == id {
            m.Items[i].X = x
            m.Items[i].Y = y
            m.Items[i].Width = width
            m.Items[i].Height = height
            m.Items[i].Rotation = rotation
            m.Items[i].ZIndex = zIndex
            return m.Save()
        }
    }
    return fmt.Errorf("item not found: %s", id)
}

// RemoveItem removes an item from the moodboard
func (m *MoodBoard) RemoveItem(id string) error {
    for i := range m.Items {
        if m.Items[i].ID == id {
            m.Items = append(m.Items[:i], m.Items[i+1:]...)
            return m.Save()
        }
    }
    return fmt.Errorf("item not found: %s", id)
}

// ListMoodBoards returns a list of all moodboards
func ListMoodBoards() ([]MoodBoard, error) {
    mu.RLock()
    defer mu.RUnlock()

    files, err := os.ReadDir(MoodBoardDir)
    if err != nil {
        if os.IsNotExist(err) {
            return []MoodBoard{}, nil
        }
        return nil, err
    }

    var boards []MoodBoard
    for _, f := range files {
        if !f.IsDir() && filepath.Ext(f.Name()) == ".json" {
            data, err := os.ReadFile(filepath.Join(MoodBoardDir, f.Name()))
            if err != nil {
                continue
            }

            var board MoodBoard
            if err := json.Unmarshal(data, &board); err != nil {
                continue
            }
            boards = append(boards, board)
        }
    }

    return boards, nil
}

// GetMoodBoard retrieves a specific moodboard by ID
func GetMoodBoard(id string) (*MoodBoard, error) {
    mu.RLock()
    defer mu.RUnlock()

    data, err := os.ReadFile(filepath.Join(MoodBoardDir, id+".json"))
    if err != nil {
        return nil, err
    }

    var board MoodBoard
    if err := json.Unmarshal(data, &board); err != nil {
        return nil, err
    }

    return &board, nil
}

// DeleteMoodBoard removes a moodboard and its thumbnail
func DeleteMoodBoard(id string) error {
    mu.Lock()
    defer mu.Unlock()

    // Remove the moodboard file
    if err := os.Remove(filepath.Join(MoodBoardDir, id+".json")); err != nil {
        return err
    }

    // Remove the thumbnail if it exists
    os.Remove(filepath.Join(ThumbnailDir, id+".jpg"))
    return nil
}
