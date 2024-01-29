

let points = []

  let a = [128, 128]
  let b = [256, 256]
  var tapCount = 0

  let midCount = 6
  let path = []
  let pathBisect = []

  let p = 32
  let s = 352
  let c = [s + p, s + p]
  // radians per second
  let speed
  
  let advanceTimeoutMillis = 2000
  let lastAdvanceMillis = -advanceTimeoutMillis
  let manual = false

  function setup() {
  speed = PI / 16

    createCanvas(2 * (s + p), 2 * (s + p))

    for (var x = 0; x < 1024; x += 16) {
    for (var y = 0; y < 1024; y += 16) {
      if (Math.random() < 0.25) {
        points.push([x, y])
      }
    }
  }

  frameRate(24)
}


function draw() {
  background(253, 253, 253)
  clear()

  if (!manual && advanceTimeoutMillis <= millis() - lastAdvanceMillis) {
    lastAdvanceMillis = millis()
      let r = speed * millis() / 1000.0
      
      a = [c[0] + s * cos(r), c[1] + s * sin(r)]
      b = [c[0] - s * cos(r), c[1] - s * sin(r)]
      let na = PI / 4 * noise(0.001 * a[0], 0.001 * a[1])
      let nb = PI / 4 * noise(0.001 * b[0], 0.001 * b[1])
      //console.log(na)
      a = [c[0] + s * cos(r + na), c[1] + s * sin(r - na)]
      b = [c[0] - s * cos(r - nb), c[1] - s * sin(r + nb)]
      //a = chooseClosest(a)
      //b = chooseClosest(a)
      setPath()
  }


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
    for (let i = 0; i < points.length; i += 1) {
    let point = points[i]

      let sumSq = pdist(point, a) + pdist(point, b)

      let s = minSumSq / sumSq

      ellipse(point[0], point[1], 8 * s, 8 * s)
  }

  stroke(color(220, 220, 220))
    fill(color(220, 220, 220))
    line(a[0], a[1], b[0], b[1])

    ellipse(a[0], a[1], 8, 8)
    ellipse(b[0], b[1], 8, 8)


    

    for (let i = 0; i + 1 < path.length; i += 1) {
      noFill()
      strokeWeight(1)
    stroke(color(255, 64, 255))
    line(path[i][0], path[i][1], path[i + 1][0], path[i + 1][1])
  }
  for (let i = 0; i < path.length; i += 1) {
    ellipse(path[i][0], path[i][1], 8, 8)
  }
  for (let i = 0; i < path.length; i += 1) {
    strokeWeight(1)
    stroke(color(255, 255, 255))
    fill(color(0, 0, 0))
    textSize(12)
    textAlign(LEFT, CENTER)
    text(`bisect ${pathBisect[i]}`, path[i][0] + 12, path[i][1])
  }
  
  textAlign(LEFT, TOP)
  noStroke()
  fill(color(0, 0, 0))
  textSize(12)
  text(`Linpath of ${midCount + 1} hops using latency map`, 12, 12)
}

function mousePressed() {
  if (0 <= mouseX && mouseX < width && 0 <= mouseY && mouseY < height) {
    tapCount += 1
      if (tapCount % 2 == 1) {
      a[0] = mouseX
        a[1] = mouseY
    } else {
      b[0] = mouseX
        b[1] = mouseY
    }
  
    setPath()
    manual = true
  }
}

function setPath() {
  let exclude = []
    path = [a, b]
    pathBisect = [0, 0]
    for (var i = 0; i < midCount; i += 1) {
    // find longest segment and create a mid there
    let m = [pdist(path[0], path[1]), 0]
      for (var j = 1; j + 1 < path.length; j += 1) {
      let s = [pdist(path[j], path[j + 1]), j]
        if (m[0] < s[0]) {
        m = s
      }
    }
    //console.log(m)
      let pi = chooseMid(path[m[1]], path[m[1] + 1], exclude)
      //console.log(pi)
      exclude.push(pi)
      path.splice(m[1] + 1, 0, points[pi])
      pathBisect.splice(m[1] + 1, 0, max(pathBisect[m[1]], pathBisect[m[1] + 1]) + 1)
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
  }
  )

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
