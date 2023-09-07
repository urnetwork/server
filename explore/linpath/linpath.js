

let points = []

let a = [128, 128]
let b = [256, 256]
var tapCount = 0

let midCount = 6
let path = []


function setup() {
  resizeCanvas(1024, 1024)
  
  for(var x = 0; x < 1024; x += 16) {
    for (var y = 0; y < 1024; y += 16) {
      if (Math.random() < 0.25) {
        points.push([x, y])
      }
    }
  }
}


function draw() {
  clear()
  ellipseMode(CENTER)
  
  let p = 4
  
  var minSumSq = -1
  for (var i = 0; i < points.length; i += 1) {
    let point = points[i]
    let sumSq = pdist(point, a) + pdist(point, b)
    if (minSumSq < 0 || sumSq < minSumSq) {
      minSumSq = sumSq
    }
  }
  
  
  fill(color(64, 64, 64))
  noStroke()
  for (var i = 0; i < points.length; i += 1) {
    let point = points[i]
    
    let sumSq = pdist(point, a) + pdist(point, b)
    
    let s = minSumSq / sumSq
    
    ellipse(point[0], point[1], 8 * s, 8 * s)
  }
  
  stroke(color(64, 64, 255))
  fill(color(64, 64, 255))
  line(a[0], a[1], b[0], b[1])
  
  ellipse(a[0], a[1], 8, 8)
  ellipse(b[0], b[1], 8, 8)
  
  
  noFill()
  stroke(color(255, 64, 255))
  
  for (var i = 0; i + 1 < path.length; i += 1) {
    line(path[i][0], path[i][1], path[i + 1][0], path[i + 1][1])
  }
  for (var i = 0; i < path.length; i += 1) {
    ellipse(path[i][0], path[i][1], 8, 8)
  }
}

function mousePressed() {
  tapCount += 1
  if (tapCount % 2 == 1) {
    a[0] = mouseX
    a[1] = mouseY
  } else {
    b[0] = mouseX
    b[1] = mouseY
  }
  
  let exclude = []
  path = [a, b]
  for (var i = 0; i < midCount; i += 1) {
    // find longest segment and create a mid there
    let m = [pdist(path[0], path[1]), 0]
    for (var j = 1; j + 1 < path.length; j += 1) {
      let s = [pdist(path[j], path[j + 1]), j]
      if (m[0] < s[0]) {
        m = s
      }
    }
    console.log(m)
    let pi = chooseMid(path[m[1]], path[m[1] + 1], exclude)
    console.log(pi)
    exclude.push(pi)
    path.splice(m[1] + 1, 0, points[pi])
  }
}

function pdist(p0, p1) {
  let p = 4
  let dx = p0[0] - p1[0]
  let dy = p0[1] - p1[1]
  return Math.pow(dx * dx + dy * dy, p / 2)
}

function chooseMid(p0, p1, exclude) {
  
  let p = 32
  
  var weighted = []
  
  
  for (var i = 0; i < points.length; i += 1) {
    if (exclude.includes(i)) {
      continue
    }
    let point = points[i]
    let sumSq = pdist(point, p0) + pdist(point, p1)
    
    weighted.push([sumSq, i])
  }
  
  weighted.sort(function(w0, w1) {
    if (w0[0] < w1[0]) {
      return -1
    } else if (w1[0] <  w0[0]) {
      return 1
    } else {
      return 0
    }
  })
  
  //return weighted[0][1]
  
  let minSumSq = weighted[0][0]
  
  var net = 0
  for (var i = 0; i < weighted.length; i += 1) {
    net += Math.pow(minSumSq / weighted[i][0], p)
  }
  
  let s = Math.random() * net
  
  var snet = 0
  for (var i = 0; i < weighted.length; i += 1) {
    snet += Math.pow(minSumSq / weighted[i][0], p)
    if (s < snet) {
      return weighted[i][1]
    }
  }
}
