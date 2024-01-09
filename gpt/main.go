package main

import (
    "context"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
)

// get path to binary
// node <dir>/innerhtml/index.js url timeout

// path, err := os.Executable()
// dir := filepath.Dir(path)
// innerHtmlPath := fmt.Sprintf("%s/innerhtml/index.js")


// # https://stackoverflow.com/questions/36399848/install-node-in-dockerfile
// ENV NODE_VERSION=v20.10.0
// RUN apt install -y curl
// RUN curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.39.7/install.sh | bash
// ENV NVM_DIR=/root/.nvm
// RUN . "$NVM_DIR/nvm.sh" && nvm install ${NODE_VERSION}
// RUN . "$NVM_DIR/nvm.sh" && nvm use ${NODE_VERSION}
// RUN . "$NVM_DIR/nvm.sh" && nvm alias default ${NODE_VERSION}
// ENV PATH="/root/.nvm/versions/node/${NODE_VERSION}/bin/:${PATH}"
// RUN node --version
// RUN npm --version


func main() {
    ctx := context.Background()

    path, err := os.Executable()
    if err != nil {
        panic(err)
    }
    dir := filepath.Dir(path)
    innerHtmlPath := fmt.Sprintf("%s/innerhtml/index.js", dir)

    fmt.Printf("PATH: %s\n", innerHtmlPath)

    c := exec.CommandContext(ctx, "node", innerHtmlPath, "https://bringyour.com/privacy", "15000")
    out, err := c.Output()
    if err != nil {
        panic(err)
    }
    fmt.Printf("%s\n", out)
}
