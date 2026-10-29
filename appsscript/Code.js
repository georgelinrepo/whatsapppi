function doPost(e) {
  const props = PropertiesService.getScriptProperties();
  const secret = props.getProperty('FEED_SECRET');
  let body;
  try {
    body = JSON.parse(e.postData.contents);
  } catch (err) {
    return ContentService.createTextOutput('bad request');
  }
  if (!secret || body.secret !== secret) {
    return ContentService.createTextOutput('forbidden');
  }
  const id = String(body.id || '').slice(0, 40);
  if (!id) {
    return ContentService.createTextOutput('bad request');
  }
  const lock = LockService.getScriptLock(); // a retry can arrive while the first request still runs
  lock.waitLock(30000);
  try {
    if (props.getProperty('LAST_FEED_ID') === id) {
      return ContentService.createTextOutput('ok'); // retry of a feed already emailed
    }
    const me = Session.getEffectiveUser().getEmail(); // only ever the script's owner
    const date = String(body.date || '').slice(0, 20);
    const text = String(body.text || '');
    if (Utilities.newBlob(text).getBytes().length > 200000) {
      return ContentService.createTextOutput('too large'); // never truncate: it would cut the footer
    }
    MailApp.sendEmail({ to: me, subject: 'WA-FEED ' + date, body: text });
    props.setProperty('LAST_FEED_ID', id);
    return ContentService.createTextOutput('ok');
  } finally {
    lock.releaseLock();
  }
}
