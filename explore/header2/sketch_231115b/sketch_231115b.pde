import java.util.List;
import java.util.Collections;
import java.util.Arrays;
import java.util.Set;
import java.util.HashSet;



int m = 4;
List<String> countryList; 

Set<Integer> blanks = new HashSet<Integer>(Arrays.asList(new Integer[]{103, 104, 119, 120, 135, 136, 255}));


void setup() {
  countryList = Arrays.asList(new String[]{
    "ad", "ae", "af", "ag", "ai", "al", "am", "ao", "aq", "ar", "as", "at", "au", "aw", "ax", "az", "ba", "bb", "bd", "be", "bf", "bg", "bh", "bi", "bj", "bl", "bm", "bn", "bo", "bq", "br", "bs", "bt", "bv", "bw", "by", "bz", "ca", "cc", "cd", "cf", "cg", "ch", "ci", "ck", "cl", "cm", "cn", "co", "cr", "cu", "cv", "cw", "cx", "cy", "cz", "de", "dj", "dk", "dm", "do", "dz", "ec", "ee", "eg", "eh", "er", "es", "et", "fi", "fj", "fk", "fm", "fo", "fr", "ga", "gb", "gd", "ge", "gf", "gg", "gh", "gi", "gl", "gm", "gn", "gp", "gq", "gr", "gs", "gt", "gu", "gw", "gy", "hk", "hm", "hn", "hr", "ht", "hu", "id", "ie", "il", "im", "in", "io", "iq", "ir", "is", "it", "je", "jm", "jo", "jp", "ke", "kg", "kh", "ki", "km", "kn", "kp", "kr", "kw", "ky", "kz", "la", "lb", "lc", "li", "lk", "lr", "ls", "lt", "lu", "lv", "ly", "ma", "mc", "md", "me", "mf", "mg", "mh", "mk", "ml", "mm", "mn", "mo", "mp", "mq", "mr", "ms", "mt", "mu", "mv", "mw", "mx", "my", "mz", "na", "nc", "ne", "nf", "ng", "ni", "nl", "no", "np", "nr", "nu", "nz", "om", "pa", "pe", "pf", "pg", "ph", "pk", "pl", "pm", "pn", "pr", "ps", "pt", "pw", "py", "qa", "re", "ro", "rs", "ru", "rw", "sa", "sb", "sc", "sd", "se", "sg", "sh", "si", "sj", "sk", "sl", "sm", "sn", "so", "sr", "ss", "st", "sv", "sx", "sy", "sz", "tc", "td", "tf", "tg", "th", "tj", "tk", "tl", "tm", "tn", "to", "tr", "tt", "tv", "tw", "tz", "ua", "ug", "um", "us", "uy", "uz", "va", "vc", "ve", "vg", "vi", "vn", "vu", "wf", "ws", "ye", "yt", "za", "zm", "zw"
  });
  Collections.shuffle(countryList);
  
  frameRate(1);
}

void settings() {

  size(m * 768, m * 768);
}

void draw() {
  background(color(253, 253, 253));
  
  int w = 16;
  int h = 16;
  
  float s = min(width / float(w), height / float(h));
  
  float xOff = (width - w * s) / 2.0;
  float yOff = (height - h * s) / 2.0;
  
  int i = 0;
  int j = 0;
  for (int x = 0; x < w; x += 1) {
    for (int y = 0; y < h; y += 1, i += 1) {
      if (blanks.contains(i)) {
      } else if (j < countryList.size()) {
        String countryCode = countryList.get(j);
        
        PImage img = loadImage(String.format("/Users/brien/Desktop/world/%s-circle-%d.png", countryCode, m * 48));
        image(img, s * x + xOff, s * y + yOff, s, s);
        
        /*
        textAlign(CENTER, CENTER);
        //stroke(color(255, 255, 255));
        //strokeWeight(2.0);
        fill(color(0, 0, 0));
        textSize(72.f);
        text("" + i, s * x + xOff + s / 2.0, s * y + yOff + s / 2.0);
        */
        
        j += 1;
      }
    }
  }
  
  save("/Users/brien/Desktop/world-sq.png");
  exit();
}
