interface Window {
  /**
   * Currently only available in Chromium based browsers:
   * https://developer.mozilla.org/en-US/docs/Web/API/DocumentPictureInPicture
   */
  documentPictureInPicture?: DocumentPictureInPicture;
}

interface DocumentPictureInPicture extends EventTarget {
  window?: Window;
  requestWindow(options?: { width: number; height: number }): Promise<Window>;
}
