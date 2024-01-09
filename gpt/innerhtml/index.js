import puppeteer from 'puppeteer';
import {TimeoutError} from 'puppeteer';

(async () => {

  const url = process.argv[2]
  const timeoutMillis = parseInt(process.argv[3])
  

  // Launch the browser and open a new blank page
  const browser = await puppeteer.launch({
    headless: "new",
    args: ['--lang=en-US', '--no-sandbox', '--disable-gpu']
  });
  const page = await browser.newPage();

  // Chrome on macOS
  const customUA = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36';
 
  // Set custom user agent
  await page.setUserAgent(customUA);

  await page.setExtraHTTPHeaders({
      'Accept-Language': 'en'
  });

  // Navigate the page to a URL
  await page.goto(url);

  // Set screen size
  await page.setViewport({width: 1920, height: 1080});

try {
  const bodyEnSelector = await page.waitForSelector(
    'body:lang(en)',
    {timeout: timeoutMillis}
  );
  const html = await bodyEnSelector?.evaluate(el => el.innerHTML);

  console.log(html);
} catch (e) {
  if (e instanceof TimeoutError) {
  } else{
    throw e;
  }
}

  await browser.close();
})();
