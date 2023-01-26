


// explore pairing a client with edges and providers using a latency map
// the latency map is a measurement of edge-to-edge and provider-to-edge latency for as many pairs as possible
// the pairing does not require a complete map or consistent mapping (latency(a, b) does not have to equal latency(b, a))
// the client search is seeded with a random fringe that excludes edges/providers that do not have the fringe in their map
// hence this encourages edges and providers to promptly map and update new edges so that their probability of being chosen is maximized
// additionally the search uses a ping from the client to the fringe edges/providers
// the latency map minimizes the number of pings using an iterative deeping 

// green square = edge
// blue circle = provider
// a red border means the edge/provider was pinged in the search


// ranks the edges by closest
// draw a box to indicate the "label" of acceptable providers
// the providers are ranked inside the box




function Point(x, y) {
  this.x = x
  this.y = y
}

function EdgeFilter(center, w, h) {
  this.center = center
  this.w = w
  this.h = h
}

function Latency(fixed, variable) {
  this.fixed = fixed
  this.variable = variable
}

function LatencyBounds(fixed, variable) {
  this.fixed = fixed
  this.variable = variable
}


function uniform(min, max) {
  let u = Math.random()
  return (1 - u) * min + u * max
}

function sumdsq(a, b) {
  let sum = 0
  for (let [edgeIdA, latencyA] of a) {
    let latencyB = b.get(edgeIdA)
    if (latencyB === undefined) {
      return undefined
    }
    let d = latencyA - latencyB
    sum += d * d
  }
  return sum
}


const DRAG_NONE = 0
const DRAG_CENTER = 1
const DRAG_X0 = 2
const DRAG_X1 = 3
const DRAG_Y0 = 4
const DRAG_Y1 = 5


const dragHandleSize = 8



var edgeFilter = null
var edgeFilterDragMode = DRAG_NONE
var edgeFilterHoverDragMode = DRAG_NONE
var edgeFilterDragOrigin = null
var edgeFilterDragSnapshot = null

// id are crypto.randomUUID()
var client
const edgeCount = 75
// id -> point
var edges = new Map()
const providerCount = 250
var providers = new Map()

var clientAddedLatencyBounds = new LatencyBounds([0, 20], [0, 5])
var clientAddedLatency
var edgeAddedLatencyBounds = new LatencyBounds([0, 20], [0, 5])
var edgeAddedLatencies = new Map()
var providerAddedLatencyBounds = new LatencyBounds([0, 20], [0, 5])
var providerAddedLatencies = new Map()

var edgeToEdgeLatencies = new Map()
var providerToEdgeLatencies = new Map()


var providersInFilter = []
var searchedClientToEdgeLatencies = new Map()
var searchedEdgeRanks = new Map()
var closestEdgeId
var searchedClientToProviderLatencies = new Map()
var searchedProviderRanks = new Map()
var closestProviderId
// 0..1
var adjustedEdgeRanks = new Map()
var adjustedProviderRanks = new Map()



function update() {
  updateProvidersInFilter()
  updateVariableLatencies()
  findClosestEdge()
  findClosestProvider()
  rankEdges()
  rankProviders()
}

function updateProvidersInFilter() {
  providersInFilter = []
  for (let [providerId, provider] of providers) {
    if (edgeFilter.center.x - edgeFilter.w / 2 <= provider.x && provider.x < edgeFilter.center.x + edgeFilter.w / 2 && 
        edgeFilter.center.y - edgeFilter.h / 2 <= provider.y && provider.y < edgeFilter.center.y + edgeFilter.h / 2) {
      providersInFilter.push(providerId)
    }
  }
}


function updateLatencyMaps() {
  edgeToEdgeLatencies = new Map()
  for (let [edgeIdA, edgeA] of edges) {
    let edgeLatencies = new Map()
    for (let [edgeIdB, edgeB] of edges) {
      let d
      if (edgeIdA == edgeIdB) {
        d = 0
      } else {
        let dx = edgeA.x - edgeB.x
        let dy = edgeA.y - edgeB.y
        d = Math.sqrt(dx * dx + dy * dy)
        let edgeAddedLatencyA = edgeAddedLatencies.get(edgeIdA)
        let edgeAddedLatencyB = edgeAddedLatencies.get(edgeIdB)
        d += edgeAddedLatencyA.fixed + edgeAddedLatencyA.variable
        d += edgeAddedLatencyB.fixed + edgeAddedLatencyB.variable
      }
      edgeLatencies.set(edgeIdB, d)
    }
    edgeToEdgeLatencies.set(edgeIdA, edgeLatencies)
  }
  
  providerToEdgeLatencies = new Map()
  for (let [providerId, provider] of providers) {
    let edgeLatencies = new Map()
    for (let [edgeId, edge] of edges) {
      let dx = provider.x - edge.x
      let dy = provider.y - edge.y
      let d = Math.sqrt(dx * dx + dy * dy)
      let providerAddedLatency = providerAddedLatencies.get(providerId)
      let edgeAddedLatency = edgeAddedLatencies.get(edgeId)
      d += providerAddedLatency.fixed + providerAddedLatency.variable
      d += edgeAddedLatency.fixed + edgeAddedLatency.variable
      edgeLatencies.set(edgeId, d)
    }
    providerToEdgeLatencies.set(providerId, edgeLatencies)
  }
}


function updateVariableLatencies() {
  clientAddedLatency.variable = uniform(clientAddedLatencyBounds.variable[0], clientAddedLatencyBounds.variable[1])
  for (let [edgeId, edgeAddedLatency] of edgeAddedLatencies) {
    edgeAddedLatency.variable = uniform(edgeAddedLatencyBounds.variable[0], edgeAddedLatencyBounds.variable[1])
  }
  for (let [providerId, providerAddedLatency] of providerAddedLatencies) {
    providerAddedLatency.variable = uniform(providerAddedLatencyBounds.variable[0], providerAddedLatencyBounds.variable[1])
  }
  
  
}

function findClosestEdge() {
  // iteratively deepen and use the updated fringe to make improve each iteration
  
  let pingEdge = function(edgeId) {
    let edge = edges.get(edgeId)
    let dx = client.x - edge.x
    let dy = client.y - edge.y
    let d = Math.sqrt(dx * dx + dy * dy)
    let edgeAddedLatency = edgeAddedLatencies.get(edgeId)
    d += clientAddedLatency.fixed + clientAddedLatency.variable
    d += edgeAddedLatency.fixed + edgeAddedLatency.variable
    return d
  }
  
  const fringeSize = 3
  let clientToEdgeLatencies = new Map()
  // seed the fringe
  let shuffledEdgeIds = [...edges.keys()].sort(() => 0.5 - Math.random())
  for (let i = 0; i < fringeSize && i < shuffledEdgeIds.length; i += 1) {
    let edgeId = shuffledEdgeIds[i]
    clientToEdgeLatencies.set(edgeId, pingEdge(edgeId))
  }
  
  let minLatency = -1
  while (true) {
    let edgeRanks = new Map()
    for (let [edgeId, edgeLatencies] of edgeToEdgeLatencies) {
      if (clientToEdgeLatencies.has(edgeId)) {
        continue
      }
      let rank = sumdsq(clientToEdgeLatencies, edgeLatencies)
      if (rank === undefined) {
        continue
      }
      edgeRanks.set(edgeId, rank)
    }
    
    // ping the next fringe
    // if no improvement, stop
    
    let orderedEdgeIds = [...edgeRanks.keys()].sort(function (edgeIdA, edgeIdB) {
      return edgeRanks.get(edgeIdA) - edgeRanks.get(edgeIdB)
    })
    for (let i = 0; i < fringeSize && i < orderedEdgeIds.length; i += 1) {
      let edgeId = orderedEdgeIds[i]
      clientToEdgeLatencies.set(edgeId, pingEdge(edgeId))
    }
    let nextMinLatency = Math.min(...clientToEdgeLatencies.values())
    if (0 <= minLatency && minLatency <= nextMinLatency) {
      break
    }
    minLatency = nextMinLatency
  }
  
  searchedClientToEdgeLatencies = clientToEdgeLatencies
  let orderedEdgeIds = [...clientToEdgeLatencies.keys()].sort(function (edgeIdA, edgeIdB) {
    return clientToEdgeLatencies.get(edgeIdA) - clientToEdgeLatencies.get(edgeIdB)
  })
  closestEdgeId = orderedEdgeIds[0]
}

function findClosestProvider() {
  // iteratively deepen from the searchedClientToEdgeLatencies
  
  let pingProvider = function(providerId) {
    let provider = providers.get(providerId)
    let dx = client.x - provider.x
    let dy = client.y - provider.y
    let d = Math.sqrt(dx * dx + dy * dy)
    let providerAddedLatency = providerAddedLatencies.get(providerId)
    d += clientAddedLatency.fixed + clientAddedLatency.variable
    d += providerAddedLatency.fixed + providerAddedLatency.variable
    return d
  }
  
  const fringeSize = 3
  let clientToProviderLatencies = new Map()
  // seed the fringe
  let shuffledProviderIds = [...providersInFilter].sort(() => 0.5 - Math.random())
  for (let i = 0; i < fringeSize && i < shuffledProviderIds.length; i += 1) {
    let providerId = shuffledProviderIds[i]
    clientToProviderLatencies.set(providerId, pingProvider(providerId))
  }
  
  let minLatency = -1
  while (true) {
    let providerRanks = new Map()
    // note there is no change in this each iteration
    for (let providerId of providersInFilter) {
      if (clientToProviderLatencies.has(providerId)) {
        continue
      }
      let edgeLatencies = providerToEdgeLatencies.get(providerId)
      let rank = sumdsq(searchedClientToEdgeLatencies, edgeLatencies)
      if (rank === undefined) {
        continue
      }
      providerRanks.set(providerId, rank)
    }
    
    // ping the next fringe
    // if no improvement, stop
    
    let orderedProviderIds = [...providerRanks.keys()].sort(function (providerIdA, providerIdB) {
      return providerRanks.get(providerIdA) - providerRanks.get(providerIdB)
    })
    for (let i = 0; i < fringeSize && i < orderedProviderIds.length; i += 1) {
      let providerId = orderedProviderIds[i]
      clientToProviderLatencies.set(providerId, pingProvider(providerId))
    }
    let nextMinLatency = Math.min(...clientToProviderLatencies.values())
    if (0 <= minLatency && minLatency <= nextMinLatency) {
      break
    }
    minLatency = nextMinLatency
  }
  
  searchedClientToProviderLatencies = clientToProviderLatencies
  let orderedProviderIds = [...clientToProviderLatencies.keys()].sort(function (providerIdA, providerIdB) {
    return clientToProviderLatencies.get(providerIdA) - clientToProviderLatencies.get(providerIdB)
  })
  closestProviderId = orderedProviderIds[0]
}

function rankEdges() {
  searchedEdgeRanks = new Map()
  for (let [edgeId, edgeLatencies] of edgeToEdgeLatencies) {
    let rank = sumdsq(searchedClientToEdgeLatencies, edgeLatencies)
    if (rank === undefined) {
      continue
    }
    searchedEdgeRanks.set(edgeId, rank)
  }
    
  let minRank = Math.min(...searchedEdgeRanks.values())
  let maxRank = Math.max(...searchedEdgeRanks.values())
  
  adjustedEdgeRanks = new Map()
  for (let edgeId of edges.keys()) {
    let rank = searchedEdgeRanks.get(edgeId)
    let adjustedRank
    if (rank === undefined) {
      adjustedRank = 0
    } else if (minRank == maxRank) {
      adjustedRank = 1
    } else {
      adjustedRank = 1 - (rank - minRank) / (maxRank - minRank)
    }
    adjustedEdgeRanks.set(edgeId, adjustedRank)
  }
}

function rankProviders() {
  searchedProviderRanks = new Map()
  for (let providerId of providersInFilter) {
    let edgeLatencies = providerToEdgeLatencies.get(providerId)
    let rank = sumdsq(searchedClientToEdgeLatencies, edgeLatencies)
    if (rank === undefined) {
      continue
    }
    searchedProviderRanks.set(providerId, rank)
  }
    
  let minRank = Math.min(...searchedProviderRanks.values())
  let maxRank = Math.max(...searchedProviderRanks.values())
  
  adjustedProviderRanks = new Map()
  for (let providerId of providers.keys()) {
    let rank = searchedProviderRanks.get(providerId)
    let adjustedRank
    if (rank === undefined) {
      adjustedRank = 0
    } else if (minRank == maxRank) {
      adjustedRank = 1
    } else {
      adjustedRank = 1 - (rank - minRank) / (maxRank - minRank)
    }
    adjustedProviderRanks.set(providerId, adjustedRank)
  }
}


function setup() {
  createCanvas(1024, 1024)
  
  edgeFilter = new EdgeFilter(new Point(width / 2, height / 2), width / 2, height / 2)
  
  client = new Point(width / 2, height / 2 - 3 * height / 8)
  // todo edges
  // todo providers
  
  for (let i = 0; i < edgeCount; i += 1) {
    let edgeId = crypto.randomUUID()
    let edge = new Point(uniform(0, width), uniform(0, height))
    edges.set(edgeId, edge)
  }
  
  for (let i = 0; i < providerCount; i += 1) {
    let providerId = crypto.randomUUID()
    let provider = new Point(uniform(0, width), uniform(0, height))
    providers.set(providerId, provider)
  }
  
  // fixed latencies
  clientAddedLatency = new Latency(
    uniform(clientAddedLatencyBounds.fixed[0], clientAddedLatencyBounds.fixed[1]),
    uniform(clientAddedLatencyBounds.variable[0], clientAddedLatencyBounds.variable[1])
  )
  for (let edgeId of edges.keys()) {
    edgeAddedLatencies.set(edgeId, new Latency(
      uniform(edgeAddedLatencyBounds.fixed[0], edgeAddedLatencyBounds.fixed[1]),
      uniform(edgeAddedLatencyBounds.variable[0], edgeAddedLatencyBounds.variable[1])
    ))
  }
  for (let providerId of providers.keys()) {
    providerAddedLatencies.set(providerId, new Latency(
      uniform(providerAddedLatencyBounds.fixed[0], providerAddedLatencyBounds.fixed[1]),
      uniform(providerAddedLatencyBounds.variable[0], providerAddedLatencyBounds.variable[1])
    ))
  }
  
  // snapshot the latency map once
  // this reflects the slow updates to the map that do not capture faster added latency changes
  updateLatencyMaps()
  update()
}

function mouseMoved() {
  if (edgeFilter.center.x - edgeFilter.w / 2 <= mouseX && mouseX < edgeFilter.center.x + edgeFilter.w / 2 && 
      edgeFilter.center.y - edgeFilter.h / 2 <= mouseY && mouseY < edgeFilter.center.y + edgeFilter.h / 2) {
    if (edgeFilter.center.x - edgeFilter.w / 2 <= mouseX && mouseX < edgeFilter.center.x - edgeFilter.w / 2 + dragHandleSize) {
      edgeFilterHoverDragMode = DRAG_X0
    } else if (edgeFilter.center.x + edgeFilter.w / 2 - dragHandleSize <= mouseX && mouseX < edgeFilter.center.x + edgeFilter.w / 2) {
      edgeFilterHoverDragMode = DRAG_X1
    } else if (edgeFilter.center.y - edgeFilter.h / 2 <= mouseY && mouseY < edgeFilter.center.y - edgeFilter.h / 2 + dragHandleSize) {
      edgeFilterHoverDragMode = DRAG_Y0
    } else if (edgeFilter.center.y + edgeFilter.h / 2 - dragHandleSize <= mouseY && mouseY < edgeFilter.center.y + edgeFilter.h / 2) {
      edgeFilterHoverDragMode = DRAG_Y1
    } else {
      edgeFilterHoverDragMode = DRAG_CENTER
    } 
  } else {
    edgeFilterHoverDragMode = DRAG_NONE
  }
}

function mousePressed() {
  if (edgeFilter.center.x - edgeFilter.w / 2 <= mouseX && mouseX < edgeFilter.center.x + edgeFilter.w / 2 && 
      edgeFilter.center.y - edgeFilter.h / 2 <= mouseY && mouseY < edgeFilter.center.y + edgeFilter.h / 2) {
    edgeFilterDragOrigin = new Point(mouseX, mouseY)
    edgeFilterDragSnapshot = new EdgeFilter(new Point(edgeFilter.center.x, edgeFilter.center.y), edgeFilter.w, edgeFilter.h)
    if (edgeFilter.center.x - edgeFilter.w / 2 <= mouseX && mouseX < edgeFilter.center.x - edgeFilter.w / 2 + dragHandleSize) {
      edgeFilterDragMode = DRAG_X0
    } else if (edgeFilter.center.x + edgeFilter.w / 2 - dragHandleSize <= mouseX && mouseX < edgeFilter.center.x + edgeFilter.w / 2) {
      edgeFilterDragMode = DRAG_X1
    } else if (edgeFilter.center.y - edgeFilter.h / 2 <= mouseY && mouseY < edgeFilter.center.y - edgeFilter.h / 2 + dragHandleSize) {
      edgeFilterDragMode = DRAG_Y0
    } else if (edgeFilter.center.y + edgeFilter.h / 2 - dragHandleSize <= mouseY && mouseY < edgeFilter.center.y + edgeFilter.h / 2) {
      edgeFilterDragMode = DRAG_Y1
    } else {
      edgeFilterDragMode = DRAG_CENTER
    } 
  } else {
    edgeFilterDragMode = DRAG_NONE
    client = new Point(mouseX, mouseY)
    update()
  }
}

function mouseReleased() {
  edgeFilterDragMode = DRAG_NONE
}

function mouseDragged() {
  if (edgeFilterDragMode == DRAG_CENTER) {
    edgeFilter.center = new Point(
      edgeFilterDragSnapshot.center.x + mouseX - edgeFilterDragOrigin.x,
      edgeFilterDragSnapshot.center.y + mouseY - edgeFilterDragOrigin.y
    )
  } else if (edgeFilterDragMode == DRAG_X0) {
    edgeFilter.w = edgeFilterDragSnapshot.w - (mouseX - edgeFilterDragOrigin.x) * 2
  } else if (edgeFilterDragMode == DRAG_X1) {
    edgeFilter.w = edgeFilterDragSnapshot.w + (mouseX - edgeFilterDragOrigin.x) * 2
  } else if (edgeFilterDragMode == DRAG_Y0) {
    edgeFilter.h = edgeFilterDragSnapshot.h - (mouseY - edgeFilterDragOrigin.y) * 2
  } else if (edgeFilterDragMode == DRAG_Y1) {
    edgeFilter.h = edgeFilterDragSnapshot.h + (mouseY - edgeFilterDragOrigin.y) * 2
  } else {
    client = new Point(mouseX, mouseY)
  }
  update()
}

function draw() {
  background(color(255, 255, 255))
  
  strokeWeight(1)
  
  noFill()
  stroke(color(220, 220, 220))
  rectMode(CORNER)
  rect(0, 0, width, height)
  
  drawFilter()
  
  ellipseMode(CENTER)  
  for (let providerId of providersInFilter) {
    let provider = providers.get(providerId)
    let providerAddedLatency = providerAddedLatencies.get(providerId)
    
    let s = 10 + 2 * providerAddedLatency.fixed
    
    if (providerId == closestProviderId) {
      fill(color(60, 60, 255))
      stroke(color(255, 0, 0))
      strokeWeight(10)
      ellipse(provider.x, provider.y, s, s)
    } else if (searchedClientToProviderLatencies.has(providerId)) {
      // the provider was pinged
      fill(color(60, 60, 255, lerp(30, 255, adjustedProviderRanks.get(providerId))))
      stroke(color(255, 0, 0))
      strokeWeight(2)
      ellipse(provider.x, provider.y, s, s)
    } else {
      fill(color(60, 60, 255, lerp(30, 255, adjustedProviderRanks.get(providerId))))
      noStroke()
      ellipse(provider.x, provider.y, s, s)
    }
  }
  
  
  rectMode(CENTER)
  for (let [edgeId, edge] of edges) {
    let edgeAddedLatency = edgeAddedLatencies.get(edgeId)
    
    let s = 10 + 2 * edgeAddedLatency.fixed
    
    if (edgeId == closestEdgeId) {
      fill(color(60, 255, 60))
      stroke(color(255, 0, 0))
      strokeWeight(10)
      rect(edge.x, edge.y, s, s)
    } else if (searchedClientToEdgeLatencies.has(edgeId)) {
      // the edge was pinged
      fill(color(60, 255, 60, lerp(30, 255, adjustedEdgeRanks.get(edgeId))))
      stroke(color(255, 0, 0))
      strokeWeight(2)
      rect(edge.x, edge.y, s, s)
    } else {
      fill(color(60, 255, 60, lerp(30, 255, adjustedEdgeRanks.get(edgeId))))
      noStroke()
      rect(edge.x, edge.y, s, s)
    }
  }
  
  ellipseMode(CENTER)
  let s = 20 + 2 * clientAddedLatency.fixed
  fill(color(40, 40, 40))
  noStroke()
  ellipse(client.x, client.y, 20, 20)
}

function drawFilter() {
  if (edgeFilterDragMode == DRAG_X0) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x - edgeFilter.w / 2, edgeFilter.center.y - edgeFilter.h / 2, dragHandleSize, edgeFilter.h)
  } else if (edgeFilterDragMode == DRAG_X1) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x + edgeFilter.w / 2 - dragHandleSize, edgeFilter.center.y - edgeFilter.h / 2, dragHandleSize, edgeFilter.h)
  } else if (edgeFilterDragMode == DRAG_Y0) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x - edgeFilter.w / 2, edgeFilter.center.y - edgeFilter.h / 2, edgeFilter.w, dragHandleSize)
  } else if (edgeFilterDragMode == DRAG_Y1) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x - edgeFilter.w / 2, edgeFilter.center.y + edgeFilter.h / 2 - dragHandleSize, edgeFilter.w, dragHandleSize)
  } else if (edgeFilterDragMode == DRAG_CENTER) {
    rectMode(CENTER)
    fill(color(220, 220, 220))
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
  } else if (edgeFilterHoverDragMode == DRAG_X0) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x - edgeFilter.w / 2, edgeFilter.center.y - edgeFilter.h / 2, dragHandleSize, edgeFilter.h)
  } else if (edgeFilterHoverDragMode == DRAG_X1) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x + edgeFilter.w / 2 - dragHandleSize, edgeFilter.center.y - edgeFilter.h / 2, dragHandleSize, edgeFilter.h)
  } else if (edgeFilterHoverDragMode == DRAG_Y0) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x - edgeFilter.w / 2, edgeFilter.center.y - edgeFilter.h / 2, edgeFilter.w, dragHandleSize)
  } else if (edgeFilterHoverDragMode == DRAG_Y1) {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
    rectMode(CORNER)
    fill(color(220, 220, 220))
    noStroke()
    rect(edgeFilter.center.x - edgeFilter.w / 2, edgeFilter.center.y + edgeFilter.h / 2 - dragHandleSize, edgeFilter.w, dragHandleSize)
  } else if (edgeFilterHoverDragMode == DRAG_CENTER) {
    rectMode(CENTER)
    fill(color(220, 220, 220))
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
  } else {
    rectMode(CENTER)
    noFill()
    stroke(color(220, 220, 220))
    rect(edgeFilter.center.x, edgeFilter.center.y, edgeFilter.w, edgeFilter.h)
  }
}
